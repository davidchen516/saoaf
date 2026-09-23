package registry

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func mustConnR(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	c, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func withDBR(t *testing.T, fn func(dsn string)) {
	t.Helper()
	base := os.Getenv("SAOAF_TEST_PG_DSN")
	if base == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set")
	}
	admin := mustConnR(t, base)
	ctx := context.Background()
	name := fmtDBR("registry_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create db: %v", err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	db := u.String()
	gooseUpR(t, db)
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	fn(db)
}

func fmtDBR(f string, a ...any) string { return fmt.Sprintf(f, a...) }

func gooseUpR(t *testing.T, db string) {
	t.Helper()
	bin := os.Getenv("GOOSE_BIN")
	if bin == "" {
		var err error
		if bin, err = exec.LookPath("goose"); err != nil {
			t.Skip("goose CLI not found")
		}
	}
	wd, _ := os.Getwd()
	dir := filepath.Join(wd, "..", "..", "migrations")
	out, err := exec.Command(bin, "-dir", dir, "postgres", db, "up").CombinedOutput()
	if err != nil {
		t.Fatalf("goose up: %v\n%s", err, out)
	}
}

// seedProvider inserts a DRAFT provider and returns its id.
func seedProvider(t *testing.T, conn *pgx.Conn, key string) int64 {
	t.Helper()
	var id int64
	if err := conn.QueryRow(context.Background(), `
		INSERT INTO registry.resource_provider
			(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		VALUES ($1, 'MODEL', 'ref://mmr', 'user:alice', 'spiffe://saoaf.test/ns/default/sa/mmr', 'VALIDATED', 1, 1)
		RETURNING id`, key).Scan(&id); err != nil {
		t.Fatalf("seed provider: %v", err)
	}
	return id
}

// —— 状态机全矩阵（issue 关闭判定①）——
func TestFullTransitionMatrix(t *testing.T) {
	states := []State{StateDraft, StateValidated, StatePublished, StateDeprecated, StateSuspended, StateRetired}
	legal := map[State][]State{
		StateDraft:      {StateValidated},
		StateValidated:  {StatePublished},
		StatePublished:  {StateDeprecated, StateSuspended},
		StateDeprecated: {StateRetired},
		StateSuspended:  {StatePublished, StateRetired},
		StateRetired:    {},
	}
	for _, from := range states {
		for _, to := range states {
			if from == to {
				continue
			}
			want := false
			for _, x := range legal[from] {
				if x == to {
					want = true
				}
			}
			if got := ValidateTransition(from, to); got != want {
				t.Errorf("ValidateTransition(%s→%s) = %v, want %v", from, to, got, want)
			}
		}
	}
	// every illegal transition via Transition() returns a reason-coded error
	for _, from := range states {
		for _, to := range states {
			if ValidateTransition(from, to) {
				continue
			}
			if _, err := Transition(from, to); err == nil {
				t.Errorf("Transition(%s→%s) unexpectedly succeeded", from, to)
			}
		}
	}
}

// —— 禁止字段负向（GWT#2/schema）——
func TestForbiddenFields(t *testing.T) {
	bad := []map[string]any{
		{"physical_model": "llama-70b"},
		{"price": 0.5},
		{"weight": 1.0},
		{"fallback_chain": []string{"a", "b"}},
		{"credentials": "secret"},
		{"prompt": "system prompt"},
	}
	for _, m := range bad {
		if err := CheckForbiddenFields(m); err == nil {
			t.Fatalf("forbidden field accepted: %v", m)
		}
	}
	if err := CheckForbiddenFields(map[string]any{"profile_id": "ok"}); err != nil {
		t.Fatalf("legitimate field rejected: %v", err)
	}
}

// —— Snapshot 校验负向（GWT#2 各类 reason code）——
func TestSnapshotValidationNegatives(t *testing.T) {
	now := time.Now()
	base := func() *Snapshot {
		return &Snapshot{
			ProviderID: 1, SnapshotVersion: 2, ContractVersion: "2026.09",
			Digest: Digest("payload"), Signature: "sig-abc",
			WorkloadIdentity: "spiffe://saoaf.test/ns/default/sa/mmr",
			GeneratedAt:      now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour),
		}
	}
	cases := []struct {
		name   string
		mutate func(*Snapshot)
		reason string
	}{
		{"bad digest", func(s *Snapshot) { s.Digest = "not-a-digest" }, ReasonDigestFormat},
		{"missing signature", func(s *Snapshot) { s.Signature = "" }, ReasonSignatureInvalid},
		{"missing workload", func(s *Snapshot) { s.WorkloadIdentity = "" }, ReasonWorkloadIdentity},
		{"workload mismatch", func(s *Snapshot) { s.WorkloadIdentity = "spiffe://other/x" }, ReasonWorkloadIdentity},
		{"contract major mismatch", func(s *Snapshot) { s.ContractVersion = "2025.01" }, ReasonContractMajor},
		{"valid_until before generated", func(s *Snapshot) { s.ValidUntil = s.GeneratedAt.Add(-time.Hour) }, ReasonExpired},
		{"already expired", func(s *Snapshot) { s.ValidUntil = now.Add(-time.Hour) }, ReasonExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := base()
			tc.mutate(s)
			err := ValidateSnapshot(s, map[string]any{}, "spiffe://saoaf.test/ns/default/sa/mmr", "2026", now)
			if err == nil {
				t.Fatal("accepted")
			}
			if ve, ok := err.(*ValidationError); !ok || ve.Reason != tc.reason {
				t.Fatalf("reason = %v, want %s", err, tc.reason)
			}
		})
	}
	// happy path (expected major 2026 matches 2026-09)
	if err := ValidateSnapshot(base(), map[string]any{}, "spiffe://saoaf.test/ns/default/sa/mmr", "2026", now); err != nil {
		t.Fatalf("valid snapshot rejected: %v", err)
	}
	// default: no expected-major constraint
	if err := ValidateSnapshot(base(), map[string]any{}, "", "", now); err != nil {
		t.Fatalf("valid snapshot (no constraints) rejected: %v", err)
	}
}

// —— DB：CAS 并发（GWT#4）——
func TestDBConcurrentCAS(t *testing.T) {
	withDBR(t, func(db string) {
		conn := mustConnR(t, db)
		ctx := context.Background()
		seedProvider(t, conn, "prov-cas")
		store := Store{DSN: db}
		var successes, conflicts atomic.Int64
		var wg sync.WaitGroup
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err := store.TransitionProvider(ctx, "prov-cas", 1, StateValidated, StatePublished)
				if err == nil {
					successes.Add(1)
				} else if errorsIs(err, ErrRevisionConflict) {
					conflicts.Add(1)
				} else {
					t.Errorf("unexpected: %v", err)
				}
			}()
		}
		wg.Wait()
		if successes.Load() != 1 || conflicts.Load() != 1 {
			t.Fatalf("successes=%d conflicts=%d, want 1/1", successes.Load(), conflicts.Load())
		}
	})
}

func errorsIs(err, target error) bool { return errors.Is(err, target) }

// —— DB：快照乱序/同版本不同 digest（GWT#2/GWT#3）——
func TestDBSnapshotOrderAndDuplicate(t *testing.T) {
	withDBR(t, func(db string) {
		conn := mustConnR(t, db)
		ctx := context.Background()
		pid := seedProvider(t, conn, "prov-snap")
		store := Store{DSN: db}
		now := time.Now()
		mk := func(ver int, digest string) *Snapshot {
			return &Snapshot{ProviderID: pid, SnapshotVersion: ver,
				ContractVersion: "2026.09", Digest: digest, Signature: "sig",
				WorkloadIdentity: "spiffe://saoaf.test/ns/default/sa/mmr",
				GeneratedAt:      now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour)}
		}
		// v1 提交
		if err := store.SubmitSnapshot(ctx, mk(1, Digest("v1"))); err != nil {
			t.Fatalf("v1: %v", err)
		}
		// 乱序：v0 ≤ max(1) → out of order
		if err := store.SubmitSnapshot(ctx, mk(0, Digest("x"))); err == nil {
			t.Fatal("out-of-order accepted")
		}
		// 同版本不同 digest → 明确拒绝（乱序或唯一冲突均覆盖语义：
		// 同版本不可能再插入）
		if err := store.SubmitSnapshot(ctx, mk(1, Digest("different"))); err == nil {
			t.Fatal("same-version-different-digest accepted")
		}
		// 同版本同 digest 重放 → 同样拒绝（幂等冲突信号）
		if err := store.SubmitSnapshot(ctx, mk(1, Digest("v1"))); err == nil {
			t.Fatal("duplicate replay accepted without conflict signal")
		}
		// 唯一约束直接验证（绕过乱序前置检查）：直接 INSERT 同 (provider, version)
		// 不同 digest 被 23505 拒绝
		nowT := time.Now()
		_, err := conn.Exec(ctx, `
			INSERT INTO registry.provider_snapshot
				(provider_id, snapshot_version, contract_version, digest, signature, workload_identity, generated_at, valid_until)
			VALUES ($1, 1, '2026.09', $2, 'sig', 'spiffe://saoaf.test/ns/default/sa/mmr', $3, $4)`,
			pid, Digest("direct-different"), nowT.Add(-time.Hour), nowT.Add(24*time.Hour))
		if err == nil {
			t.Fatal("direct same-version insert accepted — unique constraint missing")
		}
	})
}

// —— DB：已发布不可变（trigger）+ RETIRED/SUSPENDED 不在可发布候选 ——
func TestDBPublishedImmutableAndCandidates(t *testing.T) {
	withDBR(t, func(db string) {
		conn := mustConnR(t, db)
		ctx := context.Background()
		store := Store{DSN: db}
		pid := seedProvider(t, conn, "prov-imm")
		// publish
		if err := store.TransitionProvider(ctx, "prov-imm", 1, StateValidated, StatePublished); err != nil {
			t.Fatalf("publish: %v", err)
		}
		// 内容不可变（trigger 拒 endpoint/owner/workload 修改）
		for _, col := range []string{"endpoint_ref", "owner_ref", "workload_identity"} {
			_, err := conn.Exec(ctx, "UPDATE registry.resource_provider SET "+col+" = 'x' WHERE id = $1", pid)
			if err == nil {
				t.Fatalf("published %s update accepted", col)
			}
		}
		// state 转换仍合法（mutable 列）
		if _, err := conn.Exec(ctx, "UPDATE registry.resource_provider SET state = 'SUSPENDED' WHERE id = $1", pid); err != nil {
			t.Fatalf("state transition rejected: %v", err)
		}
		// suspended 不在可发布候选
		cands, err := store.PublishableProviders(ctx, "MODEL")
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range cands {
			if c.ProviderKey == "prov-imm" {
				t.Fatal("SUSPENDED provider in publishable candidates")
			}
		}
		// RETIRED 同样不在
		if _, err := conn.Exec(ctx, "UPDATE registry.resource_provider SET state = 'RETIRED' WHERE id = $1", pid); err != nil {
			t.Fatal(err)
		}
		cands, _ = store.PublishableProviders(ctx, "MODEL")
		for _, c := range cands {
			if c.ProviderKey == "prov-imm" {
				t.Fatal("RETIRED provider in publishable candidates")
			}
		}
		// 恢复 PUBLISHED 后回到候选
		if _, err := conn.Exec(ctx, "UPDATE registry.resource_provider SET state = 'PUBLISHED' WHERE id = $1", pid); err != nil {
			t.Fatal(err)
		}
		cands, _ = store.PublishableProviders(ctx, "MODEL")
		found := false
		for _, c := range cands {
			if c.ProviderKey == "prov-imm" {
				found = true
			}
		}
		if !found {
			t.Fatal("PUBLISHED provider missing from candidates")
		}
	})
}

// —— DB：active pointer 唯一 + 切换（GWT#1/#7）——
func TestDBActivePointerSwitch(t *testing.T) {
	withDBR(t, func(db string) {
		conn := mustConnR(t, db)
		ctx := context.Background()
		pid := seedProvider(t, conn, "prov-ptr")
		store := Store{DSN: db}
		now := time.Now()
		mk := func(ver int) *Snapshot {
			return &Snapshot{ProviderID: pid, SnapshotVersion: ver,
				ContractVersion: "2026.09", Digest: Digest(fmt.Sprintf("v%d", ver)), Signature: "sig",
				WorkloadIdentity: "spiffe://saoaf.test/ns/default/sa/mmr",
				GeneratedAt:      now.Add(-time.Hour), ValidUntil: now.Add(24 * time.Hour)}
		}
		if err := store.SubmitSnapshot(ctx, mk(1)); err != nil {
			t.Fatal(err)
		}
		if err := store.SubmitSnapshot(ctx, mk(2)); err != nil {
			t.Fatal(err)
		}
		// activate v1
		if err := store.ActivateSnapshot(ctx, mk(1), map[string]any{}, "", "", func() time.Time { return now }); err != nil {
			t.Fatalf("activate v1: %v", err)
		}
		as, err := store.ActiveSnapshot(ctx, pid)
		if err != nil || as.SnapshotVersion != 1 {
			t.Fatalf("active = %+v err=%v", as, err)
		}
		// switch to v2 (pointer upsert — still exactly one)
		if err := store.ActivateSnapshot(ctx, mk(2), map[string]any{}, "", "", func() time.Time { return now }); err != nil {
			t.Fatalf("activate v2: %v", err)
		}
		as, err = store.ActiveSnapshot(ctx, pid)
		if err != nil || as.SnapshotVersion != 2 {
			t.Fatalf("active after switch = %+v err=%v", as, err)
		}
		// exactly one pointer row
		var n int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM registry.provider_active_pointer WHERE provider_id = $1`, pid).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("pointer rows = %d, want 1", n)
		}
	})
}
