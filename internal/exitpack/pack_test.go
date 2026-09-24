package exitpack

// I14 tests: 完备性负向套件（四类缺失逐项 reason code）、导出 digest
// 篡改检测（红→绿）、并发激活、历史不可变负向、到期扫描与风险视图、
// 幂等重放。真实 PostgreSQL。

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func withDBX(t *testing.T, fn func(dsn string, store Store)) {
	t.Helper()
	base := os.Getenv("SAOAF_TEST_PG_DSN")
	if base == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	name := fmt.Sprintf("exitpack_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	dsn := u.String()
	bin := os.Getenv("GOOSE_BIN")
	if bin == "" {
		if bin, err = exec.LookPath("goose"); err != nil {
			t.Skip("goose CLI not found")
		}
	}
	wd, _ := os.Getwd()
	if out, err := exec.Command(bin, "-dir", filepath.Join(wd, "..", "..", "migrations"),
		"postgres", dsn, "up").CombinedOutput(); err != nil {
		t.Fatalf("goose up: %v\n%s", err, out)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	testPoolHolder = pool
	fn(dsn, Store{Pool: pool})
}

func completePack(key, vendor string) *Pack {
	until := time.Now().Add(24 * time.Hour)
	return &Pack{
		PackKey: key, Vendor: vendor,
		OwnerRef: "group:platform", SubstituteProvider: "prov-substitute",
		RecoverySteps: []string{"provision substitute", "re-point bindings", "verify plans"},
		EvidenceRefs:  []string{"evidence:drill-42", "evidence:coverage-check"},
		Checklist:     map[string]any{"data_export": "verified", "config_export": "verified"},
		ValidUntil:    &until,
		CreatedBy:     "user:ops",
	}
}

func isErrX(err error, reason string) bool {
	if err == nil {
		return false
	}
	ev, ok := err.(*ErrValidation)
	return ok && ev.Reason == reason
}

// GWT#2 完备性负向套件：四类缺失逐项拒绝 reason code 明确。
func TestCompletenessNegatives(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(*Pack)
		reason string
	}{
		{"missing owner", func(p *Pack) { p.OwnerRef = "" }, ReasonMissingOwner},
		{"missing substitute", func(p *Pack) { p.SubstituteProvider = "" }, ReasonMissingSubstitute},
		{"missing recovery", func(p *Pack) { p.RecoverySteps = nil }, ReasonMissingRecovery},
		{"missing evidence", func(p *Pack) { p.EvidenceRefs = nil }, ReasonMissingEvidence},
	}
	for _, tc := range cases {
		p := completePack("neg-pack", "vendor-neg")
		tc.mut(p)
		err := ValidateCompleteness(p)
		if !isErrX(err, tc.reason) {
			t.Fatalf("%s: got %v, want reason %s", tc.name, err, tc.reason)
		}
	}
	if err := ValidateCompleteness(completePack("neg-pack", "vendor-neg")); err != nil {
		t.Fatalf("complete pack rejected: %v", err)
	}
}

// GWT#1 + GWT#3：验证 → 激活 → 幂等重放；导出 digest 与 manifest 落库。
func TestValidateActivateIdempotent(t *testing.T) {
	withDBX(t, func(dsn string, store Store) {
		ctx := context.Background()
		p := completePack("pack-a", "vendor-a")
		if err := store.CreateDraft(ctx, p); err != nil {
			t.Fatal(err)
		}
		if p.Revision != 1 || p.State != StateDraft {
			t.Fatalf("draft = rev %d state %s", p.Revision, p.State)
		}
		if err := store.Validate(ctx, "pack-a", 1); err != nil {
			t.Fatalf("validate: %v", err)
		}
		got, err := store.Get(ctx, "pack-a", 1)
		if err != nil {
			t.Fatal(err)
		}
		if got.State != StateValidated || got.Digest == "" {
			t.Fatalf("validated = %s digest=%q", got.State, got.Digest)
		}
		// the export manifest round-trips and verifies
		if err := VerifyExport([]byte(manifestJSON(t, got)), got.Digest); err != nil {
			t.Fatalf("verify: %v", err)
		}
		if err := store.Activate(ctx, "pack-a", 1); err != nil {
			t.Fatalf("activate: %v", err)
		}
		// idempotent re-activation（GWT#3: no-op）
		if err := store.Activate(ctx, "pack-a", 1); err != nil {
			t.Fatalf("re-activate must be idempotent: %v", err)
		}
		got, _ = store.Get(ctx, "pack-a", 1)
		if got.State != StateActive {
			t.Fatalf("state = %s, want ACTIVE", got.State)
		}
	})
}

func manifestJSON(t *testing.T, p *Pack) string {
	t.Helper()
	rows, err := storePool(t).Query(context.Background(),
		`SELECT export_manifest FROM saoaf.exit_pack WHERE pack_key = $1 AND revision = $2`,
		p.PackKey, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no manifest row")
	}
	var s string
	_ = rows.Scan(&s)
	return s
}

var testPoolHolder *pgxpool.Pool

func storePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	return testPoolHolder
}

// GWT#8 篡改检测（红 → 绿）：任何字节改动 → 验证失败。
func TestExportTamperDetection(t *testing.T) {
	p := completePack("tamper-pack", "vendor-t")
	manifest, digest, err := BuildExport(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyExport(manifest, digest); err != nil {
		t.Fatalf("clean verify failed: %v", err)
	}
	// tamper: flip one byte
	tampered := append([]byte{}, manifest...)
	tampered[len(tampered)/2] ^= 0xFF
	if err := VerifyExport(tampered, digest); err == nil {
		t.Fatal("tampered manifest verified — digest check broken")
	}
	// also: same manifest against a different digest
	if err := VerifyExport(manifest, "sha256:"+fmt.Sprintf("%064d", 1)); err == nil {
		t.Fatal("digest mismatch not detected")
	}
}

// 导出内容零敏感（红）：含密钥字段的清单被扫描拒绝。
func TestExportForbiddenScan(t *testing.T) {
	bad := &Pack{PackKey: "x", Vendor: "v", OwnerRef: "o", SubstituteProvider: "s",
		RecoverySteps: []string{"step"}, EvidenceRefs: []string{"e"},
		Checklist: map[string]any{"api_key": "should-not-be-here"}}
	m, _, err := BuildExport(bad)
	if err != nil {
		t.Fatal(err)
	}
	if err := ScanExportForForbidden(m); err == nil {
		t.Fatal("forbidden export content accepted")
	}
	good := completePack("scan-pack", "vendor-s")
	mg, _, err := BuildExport(good)
	if err != nil {
		t.Fatal(err)
	}
	if err := ScanExportForForbidden(mg); err != nil {
		t.Fatalf("clean export rejected: %v", err)
	}
}

// GWT#4 并发：两个 VALIDATED revision 并发激活——部分唯一索引保证
// 恰一个 ACTIVE；败者 SUPERSEDED 路径已被胜者完成。
func TestConcurrentActivateSingleActive(t *testing.T) {
	withDBX(t, func(dsn string, store Store) {
		ctx := context.Background()
		for _, key := range []string{"pack-c1", "pack-c2"} {
			p := completePack(key, "vendor-c")
			if err := store.CreateDraft(ctx, p); err != nil {
				t.Fatal(err)
			}
			if err := store.Validate(ctx, key, 1); err != nil {
				t.Fatalf("validate %s: %v", key, err)
			}
		}
		var wg sync.WaitGroup
		var okCount, errCount int
		var mu sync.Mutex
		for _, key := range []string{"pack-c1", "pack-c2"} {
			wg.Add(1)
			go func(k string) {
				defer wg.Done()
				if err := store.Activate(ctx, k, 1); err == nil {
					mu.Lock()
					okCount++
					mu.Unlock()
				} else {
					mu.Lock()
					errCount++
					mu.Unlock()
				}
			}(key)
		}
		wg.Wait()
		// both may succeed (second supersedes first) — the invariant is
		// exactly ONE ACTIVE row for the vendor
		var active int
		_ = testPoolHolder.QueryRow(context.Background(),
			`SELECT count(*) FROM saoaf.exit_pack WHERE vendor = 'vendor-c' AND state = 'ACTIVE'`).Scan(&active)
		if active != 1 {
			t.Fatalf("ACTIVE rows = %d, want exactly 1 (并发恰一 active)", active)
		}
		_ = okCount
		_ = errCount
	})
}

// GWT#7 历史不可变（负向）：已验证 revision 的内容字段改写被 trigger 拒绝。
func TestVerifiedRevisionImmutable(t *testing.T) {
	withDBX(t, func(dsn string, store Store) {
		ctx := context.Background()
		p := completePack("imm-pack", "vendor-imm")
		if err := store.CreateDraft(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := store.Validate(ctx, "imm-pack", 1); err != nil {
			t.Fatal(err)
		}
		_, err := testPoolHolder.Exec(ctx, `
			UPDATE saoaf.exit_pack SET owner_ref = 'attacker' WHERE pack_key = 'imm-pack'`)
		if !isCheckViolation(err) {
			t.Fatalf("verified revision content rewrite must be rejected, got %v", err)
		}
		_, err = testPoolHolder.Exec(ctx, `DELETE FROM saoaf.exit_pack WHERE pack_key = 'imm-pack'`)
		if !isCheckViolation(err) {
			t.Fatalf("verified revision delete must be rejected, got %v", err)
		}
	})
}

func isCheckViolation(err error) bool {
	if err == nil {
		return false
	}
	var pgErr interface{ SQLState() string }
	if ok := asErr(err, &pgErr); ok {
		return pgErr.SQLState() == "23514"
	}
	return false
}

func asErr(err error, target *interface{ SQLState() string }) bool {
	if err == nil {
		return false
	}
	if st, ok := err.(interface{ SQLState() string }); ok {
		*target = st
		return true
	}
	return false
}

// GWT 到期：ACTIVE 过 valid_until → SweepExpired → EXPIRED + 风险视图。
func TestExpirySweepAndRiskView(t *testing.T) {
	withDBX(t, func(dsn string, store Store) {
		ctx := context.Background()
		past := time.Now().Add(-time.Minute)
		p := completePack("exp-pack", "vendor-exp")
		p.ValidUntil = &past
		if err := store.CreateDraft(ctx, p); err != nil {
			t.Fatal(err)
		}
		if err := store.Validate(ctx, "exp-pack", 1); err != nil {
			t.Fatal(err)
		}
		if err := store.Activate(ctx, "exp-pack", 1); err != nil {
			t.Fatal(err)
		}
		expired, err := store.SweepExpired(ctx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if len(expired) != 1 || expired[0].PackKey != "exp-pack" {
			t.Fatalf("expired = %+v", expired)
		}
		got, _ := store.Get(ctx, "exp-pack", 1)
		if got.State != StateExpired {
			t.Fatalf("state after sweep = %s", got.State)
		}
		// risk view surfaces the expired pack（I13 风险视图联动）
		risks, err := store.RiskView(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(risks) != 1 || risks[0].PackKey != "exp-pack" || risks[0].State != StateExpired {
			t.Fatalf("risk view = %+v", risks)
		}
	})
}

// GWT#7 回滚：激活上一已验证 revision（新版本有缺陷时）。
func TestRollbackToPreviousValidated(t *testing.T) {
	withDBX(t, func(dsn string, store Store) {
		ctx := context.Background()
		// v1: validate + activate
		p1 := completePack("rb-pack", "vendor-rb")
		if err := store.CreateDraft(ctx, p1); err != nil {
			t.Fatal(err)
		}
		if err := store.Validate(ctx, "rb-pack", 1); err != nil {
			t.Fatal(err)
		}
		if err := store.Activate(ctx, "rb-pack", 1); err != nil {
			t.Fatal(err)
		}
		// v2: validate + activate (supersedes v1)
		p2 := completePack("rb-pack", "vendor-rb")
		p2.SubstituteProvider = "prov-better"
		if err := store.CreateDraft(ctx, p2); err != nil {
			t.Fatal(err)
		}
		if p2.Revision != 2 {
			t.Fatalf("v2 revision = %d", p2.Revision)
		}
		if err := store.Validate(ctx, "rb-pack", 2); err != nil {
			t.Fatal(err)
		}
		if err := store.Activate(ctx, "rb-pack", 2); err != nil {
			t.Fatal(err)
		}
		got, _ := store.Get(ctx, "rb-pack", 2)
		if got.State != StateActive {
			t.Fatalf("v2 = %s", got.State)
		}
		v1, _ := store.Get(ctx, "rb-pack", 1)
		if v1.State != StateSuperseded {
			t.Fatalf("v1 = %s, want SUPERSEDED", v1.State)
		}
		// rollback: re-activate v1 — a SUPERSEDED revision moving back to
		// ACTIVE is a state-only transition（no content change, history
		// intact; the current ACTIVE v2 is superseded）
		if err := store.Activate(ctx, "rb-pack", 1); err != nil {
			t.Fatalf("rollback activate v1: %v", err)
		}
		v1b, _ := store.Get(ctx, "rb-pack", 1)
		if v1b.State != StateActive {
			t.Fatalf("v1 after rollback = %s, want ACTIVE", v1b.State)
		}
		v2b, _ := store.Get(ctx, "rb-pack", 2)
		if v2b.State != StateSuperseded {
			t.Fatalf("v2 after rollback = %s, want SUPERSEDED", v2b.State)
		}
	})
}
