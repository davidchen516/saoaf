package metrics

// I13 tests: the core acceptance matrix — 纯函数可重复（固定语料 diff=0）、
// 五类边界 fixture（未知 ≠ 0）、告警幂等（重算/重启零重复）、公式回滚不改写
// 历史、并发聚合、查询下钻三元组。真实 PostgreSQL。

import (
	"context"
	"encoding/json"
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
	"github.com/jackc/pgx/v5/pgxpool"
)

func withDBM(t *testing.T, fn func(dsn string, store Store)) {
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
	name := fmt.Sprintf("metrics_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	fn(dsn, Store{Pool: pool})
}

// fixedDataset is the 固定语料 for reproducibility (GWT#1: diff = 0).
func fixedDataset(rev int64) *Dataset {
	return &Dataset{Revision: rev, Rows: []DimRow{
		{Capability: "cap-a", Provider: "p1", Vendor: "vendor-x", Environment: "prod",
			ActiveBindings: 3, BindingsWithSubstitute: 2},
		{Capability: "cap-a", Provider: "p2", Vendor: "vendor-x", Environment: "prod",
			ActiveBindings: 1, BindingsWithSubstitute: 1},
		{Capability: "cap-b", Provider: "p3", Vendor: "vendor-y", Environment: "prod",
			ActiveBindings: 2, BindingsWithSubstitute: 0, BindingIssues: 1},
		{Capability: "", Provider: "p4", Vendor: "vendor-z", Environment: "staging",
			ActiveBindings: 1, BindingsWithSubstitute: 1},
	}}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// GWT#1: 固定数据集两次计算 diff = 0 + 追溯三元组完整。
func TestReproducibilityDiffZero(t *testing.T) {
	run1 := ComputeV1(fixedDataset(42))
	run2 := ComputeV1(fixedDataset(42))
	if mustJSON(run1) != mustJSON(run2) {
		t.Fatalf("固定语料重跑 diff != 0:\n%s\n%s", mustJSON(run1), mustJSON(run2))
	}
	for _, r := range run1 {
		if r.DatasetRevision != 42 || r.FormulaVersion != FormulaV1 ||
			r.EvidenceRef != "dataset:42@v1" {
			t.Fatalf("追溯三元组不完整: %+v", r)
		}
	}
	// spot-check a concrete value: cap-a coverage = (2+1)/(3+1) = 0.75
	for _, r := range run1 {
		if r.MetricKey == KeySubstitutionCoverage && r.Dimensions["capability"] == "cap-a" {
			if r.Status != StatusOK || r.Value == nil || *r.Value != 0.75 {
				t.Fatalf("cap-a coverage = %+v, want 0.75/OK", r)
			}
		}
	}
}

// 五类边界 fixture（GWT#2 + 关闭判定第 2 项）：空分母 / 未知供应商 /
// 重复 Provider / 失效 Binding / 过期 Snapshot —— 未知 ≠ 0。
func TestSemanticClassification(t *testing.T) {
	// 空分母: capability with no live bindings
	d := &Dataset{Revision: 1, Rows: []DimRow{
		{Capability: "cap-empty", Provider: "p", Vendor: "v", ActiveBindings: 0},
	}}
	res := ComputeV1(d)
	found := false
	for _, r := range res {
		if r.MetricKey == KeySubstitutionCoverage && r.Dimensions["capability"] == "cap-empty" {
			found = true
			if r.Status != StatusNotApplicable || r.Value != nil {
				t.Fatalf("空分母 must be NOT_APPLICABLE with nil value: %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("empty-denominator row missing")
	}

	// 未知供应商: UNKNOWN — 绝不折叠为 0
	d = &Dataset{Revision: 1, Rows: []DimRow{
		{Capability: "cap-u", Provider: "p1", Vendor: "?", UnknownVendor: true, ActiveBindings: 2},
		{Capability: "cap-u", Provider: "p2", Vendor: "known-v", ActiveBindings: 1},
	}}
	res = ComputeV1(d)
	for _, r := range res {
		if r.MetricKey == KeyVendorConcentration {
			if r.Status != StatusUnknown || r.Value != nil {
				t.Fatalf("未知供应商 must be UNKNOWN with nil value (未知 ≠ 0): %+v", r)
			}
		}
	}

	// 重复 Provider / 失效 Binding → 集中度 reason 标注 / 协议兼容：
	// 混合人口是真实比率（R1 P2-2），完全受损 → INSUFFICIENT_DATA
	d = &Dataset{Revision: 1, Rows: []DimRow{
		{Capability: "cap-d", Provider: "p1", Vendor: "v1", ActiveBindings: 2, DupProviderRows: 1},
		{Capability: "cap-d", Provider: "p1", Vendor: "v1", ActiveBindings: 1, DupProviderRows: 1},
		{Capability: "cap-e", Provider: "p2", Vendor: "v2", ActiveBindings: 1, BindingIssues: 1},
		{Capability: "cap-f", Provider: "p3", Vendor: "v3", BindingIssues: 2},
	}}
	res = ComputeV1(d)
	var sawDup, sawRatio, sawIssues bool
	for _, r := range res {
		if r.MetricKey == KeyVendorConcentration && r.StatusReason != "" {
			sawDup = true
		}
		if r.MetricKey == KeyProtocolCompat && r.Dimensions["capability"] == "cap-e" {
			if r.Status == StatusOK && r.Value != nil && *r.Value == 0.5 {
				sawRatio = true
			}
		}
		// protocol compat is a per-capability ratio (R1 P2-2): a FULLY
		// compromised capability classifies INSUFFICIENT_DATA
		if r.MetricKey == KeyProtocolCompat && r.Dimensions["capability"] == "cap-f" &&
			r.Status == StatusInsufficientData {
			sawIssues = true
		}
	}
	if !sawDup || !sawRatio || !sawIssues {
		t.Fatalf("dup(%v)/ratio(%v)/issues(%v) classification missing", sawDup, sawRatio, sawIssues)
	}
}

// GWT#3 + 数据不变量: 同一聚合任务重复触发 → 指标值不变、告警不重复。
func TestRecomputeIdempotency(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		results := ComputeV1(fixedDataset(42))
		if err := store.PersistResults(ctx, results); err != nil {
			t.Fatal(err)
		}
		// recompute: same dataset, same formula → same results persisted
		if err := store.PersistResults(ctx, ComputeV1(fixedDataset(42))); err != nil {
			t.Fatal(err)
		}
		var metricRows int
		_ = store.Pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.metric_result`).Scan(&metricRows)
		if metricRows != len(results) {
			t.Fatalf("metric rows = %d, want %d (重算不新增行)", metricRows, len(results))
		}
		// alerts: derive again — the same alerts must NOT duplicate
		var alertRows int
		_ = store.Pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.risk_alert`).Scan(&alertRows)
		// third run to prove stability
		_ = store.PersistResults(ctx, ComputeV1(fixedDataset(42)))
		var alertRowsAfter int
		_ = store.Pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.risk_alert`).Scan(&alertRowsAfter)
		if alertRows != alertRowsAfter {
			t.Fatalf("alerts duplicated on recompute: %d → %d", alertRows, alertRowsAfter)
		}
	})
}

// GWT#4: 并发聚合 Worker → 无重复行（唯一约束 + 幂等合并）。
func TestConcurrentAggregation(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		var wg sync.WaitGroup
		var firstErr atomic.Value
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := store.PersistResults(ctx, ComputeV1(fixedDataset(42))); err != nil {
					firstErr.CompareAndSwap(nil, err)
				}
			}()
		}
		wg.Wait()
		if v := firstErr.Load(); v != nil {
			t.Fatalf("concurrent persist error: %v", v)
		}
		var metricRows int
		_ = store.Pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.metric_result`).Scan(&metricRows)
		results := ComputeV1(fixedDataset(42))
		if metricRows != len(results) {
			t.Fatalf("rows after 4 concurrent workers = %d, want %d (幂等合并)", metricRows, len(results))
		}
	})
}

// GWT#7: 公式回滚——v2 缺陷切回 v1：新计算用 v1；历史行不被改写。
func TestFormulaRollbackNoHistoryRewrite(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		v1 := ComputeV1(fixedDataset(7))
		if err := store.PersistResults(ctx, v1); err != nil {
			t.Fatal(err)
		}
		// a (hypothetical) v2 writes different values for the same revision
		var v2 []MetricResult
		for _, r := range v1 {
			r2 := r
			r2.FormulaVersion = FormulaV2
			if r2.Value != nil {
				bad := *r2.Value + 1 // v2's defect
				r2.Value = &bad
			}
			v2 = append(v2, r2)
		}
		if err := store.PersistResults(ctx, v2); err != nil {
			t.Fatal(err)
		}
		// 回滚: recompute with v1 — the v2 rows must survive untouched
		if err := store.PersistResults(ctx, ComputeV1(fixedDataset(7))); err != nil {
			t.Fatal(err)
		}
		hist, err := store.History(ctx, KeySubstitutionCoverage, map[string]string{"capability": "cap-a"})
		if err != nil {
			t.Fatal(err)
		}
		var hasV1, hasV2 bool
		for _, h := range hist {
			if h.FormulaVersion == FormulaV1 {
				hasV1 = true
				if h.Value == nil || *h.Value != 0.75 {
					t.Fatalf("v1 history rewritten: %+v", h)
				}
			}
			if h.FormulaVersion == FormulaV2 {
				hasV2 = true
				if h.Value == nil || *h.Value != 1.75 {
					t.Fatalf("v2 history rewritten: %+v", h)
				}
			}
		}
		if !hasV1 || !hasV2 {
			t.Fatalf("history must contain both formula versions: %+v", hist)
		}
		// Latest under v1 (the rolled-back active formula) serves v1 values
		latest, err := store.Latest(ctx, FormulaV1, KeySubstitutionCoverage, nil, time.Time{}, time.Time{}, 10)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range latest {
			if l.FormulaVersion != FormulaV1 {
				t.Fatalf("latest under v1 returned %s", l.FormulaVersion)
			}
		}
	})
}

// 查询 API（GWT#1 下钻三元组）: Latest carries (revision, formula, evidence).
func TestQueryDrilldownTriple(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		if err := store.PersistResults(ctx, ComputeV1(fixedDataset(99))); err != nil {
			t.Fatal(err)
		}
		latest, err := store.Latest(ctx, FormulaV1, "", nil, time.Time{}, time.Time{}, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(latest) == 0 {
			t.Fatal("no results")
		}
		for _, r := range latest {
			if r.DatasetRevision == 0 || r.FormulaVersion == "" || r.EvidenceRef == "" {
				t.Fatalf("下钻三元组不完整: %+v", r)
			}
		}
		// alerts were derived and are idempotent
		n, err := store.OpenAlertCount(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal("fixed dataset (cap-b coverage 0.0 < 0.5) must raise a substitution alert")
		}
	})
}

// R1-P1 回归：提取器从真实表拉 Dataset（管道闭环——不再只有测试构造的行）。
func TestExtractDatasetFromLiveTables(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		conn := store.Pool
		// seed the FK chain: capability + provider + snapshot + 2 bindings
		if _, err := conn.Exec(ctx, `
			INSERT INTO registry.capability_definition
				(capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
			VALUES ('cap-x', 1, 1, 'MODEL', '{}', 'PUBLISHED', 'u')`); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, `
			INSERT INTO registry.resource_provider
				(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
			VALUES ('prov-x', 'MODEL', 'svc://x', 'vendor-live', 'spiffe://saoaf.test/x', 'PUBLISHED', 1, 1),
			       ('prov-y', 'MODEL', 'svc://y', 'vendor-live', 'spiffe://saoaf.test/y', 'PUBLISHED', 1, 1)`); err != nil {
			t.Fatal(err)
		}
		var xID, yID int
		if err := conn.QueryRow(ctx,
			`SELECT id FROM registry.resource_provider WHERE provider_key='prov-x'`).Scan(&xID); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRow(ctx,
			`SELECT id FROM registry.resource_provider WHERE provider_key='prov-y'`).Scan(&yID); err != nil {
			t.Fatal(err)
		}
		for i, pid := range []int{xID, yID} {
			if _, err := conn.Exec(ctx, `
				INSERT INTO registry.provider_snapshot
					(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
					 profiles, generated_at, valid_until, state)
				VALUES ($1, 1, '2026.09', 'sha256:333333333333333333333333333333333333333333333333333333333333333`+fmt.Sprint(i)+`', 's',
				       'spiffe://saoaf.test/x', '[]'::jsonb, now(), now() + interval '1 day', 'PUBLISHED')`, pid); err != nil {
				t.Fatal(err)
			}
			var snapID int64
			if err := conn.QueryRow(ctx,
				`SELECT id FROM registry.provider_snapshot WHERE provider_id = $1 ORDER BY id DESC LIMIT 1`, pid).Scan(&snapID); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Exec(ctx, `
				INSERT INTO registry.capability_binding
					(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
					 environment, scope, scope_hash, priority, state, revision, is_active, tenant_ref)
				VALUES ($1, (SELECT id FROM registry.capability_definition WHERE capability_key='cap-x'),
				       $2, $3, 'p', 'production', '{}'::jsonb, 'sha256:x', $4, 'PUBLISHED', 1, TRUE, 'tenant-x')`,
				fmt.Sprintf("bind-x%d", i), pid, snapID, 100+i); err != nil {
				t.Fatal(err)
			}
		}
		d, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(d.Rows) != 2 || d.Revision == 0 {
			t.Fatalf("extracted = %d rows rev=%d, want 2 rows rev>0", len(d.Rows), d.Revision)
		}
		for _, r := range d.Rows {
			if r.Capability != "cap-x" || r.Vendor != "vendor-live" || r.Tenant != "tenant-x" {
				t.Fatalf("extracted row = %+v", r)
			}
			if r.ActiveBindings != 1 || r.BindingsWithSubstitute != 1 {
				t.Fatalf("substitute detection broken: %+v", r)
			}
		}
		// pipeline closure: extract → compute → persist → query
		results := ComputeV1(d)
		if err := store.PersistResults(ctx, results); err != nil {
			t.Fatal(err)
		}
		latest, err := store.Latest(ctx, FormulaV1, KeySubstitutionCoverage,
			map[string]string{"capability": "cap-x"}, time.Time{}, time.Time{}, 10)
		if err != nil || len(latest) != 1 {
			t.Fatalf("dims-filtered query = %d err=%v", len(latest), err)
		}
		if latest[0].Status != StatusOK || latest[0].Value == nil || *latest[0].Value != 1.0 {
			t.Fatalf("cap-x coverage = %+v, want 1.0 (both providers are substitutes)", latest[0])
		}
		// a second extract of the UNCHANGED tables yields the same revision
		d2, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d2.Revision != d.Revision {
			t.Fatalf("unchanged input changed revision: %d → %d", d.Revision, d2.Revision)
		}
	})
}

// R1-P2-4 回归：重算刷新 last_seen_at（与文档一致），仍零重复。
func TestAlertLastSeenRefreshesWithoutDuplicates(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		results := ComputeV1(fixedDataset(42))
		if err := store.PersistResults(ctx, results); err != nil {
			t.Fatal(err)
		}
		var before time.Time
		if err := store.Pool.QueryRow(ctx, `
			SELECT last_seen_at FROM saoaf.risk_alert ORDER BY id LIMIT 1`).Scan(&before); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		if err := store.PersistResults(ctx, ComputeV1(fixedDataset(42))); err != nil {
			t.Fatal(err)
		}
		var count int
		var after time.Time
		_ = store.Pool.QueryRow(ctx, `SELECT count(*) FROM saoaf.risk_alert`).Scan(&count)
		if err := store.Pool.QueryRow(ctx, `
			SELECT last_seen_at FROM saoaf.risk_alert ORDER BY id LIMIT 1`).Scan(&after); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			t.Fatal("no alerts to test")
		}
		if !after.After(before) {
			t.Fatalf("last_seen_at not refreshed on recompute: %v → %v", before, after)
		}
		var after2 int
		_ = store.Pool.QueryRow(ctx, `SELECT count(*) FROM saoaf.risk_alert`).Scan(&after2)
		if count != after2 {
			t.Fatalf("refresh duplicated alerts: %d → %d", count, after2)
		}
	})
}

// R1-P2-2 回归：协议兼容率是真实比率（1.0-or-unknown 退化已修）。
func TestProtocolCompatibilityIsARatio(t *testing.T) {
	// cap-mixed: one healthy row + one issue row → ratio 0.5, NOT unknown
	d := &Dataset{Revision: 1, Rows: []DimRow{
		{Capability: "cap-mixed", Provider: "p1", Vendor: "v", ActiveBindings: 1},
		{Capability: "cap-mixed", Provider: "p2", Vendor: "v", BindingIssues: 1},
	}}
	res := ComputeV1(d)
	for _, r := range res {
		if r.MetricKey == KeyProtocolCompat && r.Dimensions["capability"] == "cap-mixed" {
			if r.Status != StatusOK || r.Value == nil || *r.Value != 0.5 {
				t.Fatalf("mixed population must yield ratio 0.5: %+v", r)
			}
			return
		}
	}
	t.Fatal("protocol ratio row missing")
}

// R2-P1 回归（审查探针 TestR2SuspendPathCollision + TestR2HistoryRewriteEndToEnd
// 正名）：生产 suspend（原地 UPDATE）必须 bump revision——同 revision 静默改写
// 历史曾端到端复现，击穿「固定数据集可重复」根基。
func TestRevisionBumpsOnInPlaceUpdate(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		// seed one live binding chain
		seedMetricChain(t, store, "cap-r", "prov-r", "bind-r", true)
		d1, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rev1 := d1.Revision

		// PRODUCTION suspend path: in-place UPDATE (binding/store.go's own
		// statement) — row id unchanged
		if _, err := store.Pool.Exec(ctx, `
			UPDATE registry.capability_binding
			SET state = 'SUSPENDED', revision = revision + 1
			WHERE binding_key = 'bind-r' AND state = 'PUBLISHED' AND is_active`); err != nil {
			t.Fatal(err)
		}
		d2, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d2.Revision == rev1 {
			t.Fatalf("in-place suspend did NOT bump revision (%d == %d) — history rewrite window", rev1, d2.Revision)
		}
		// the pipeline result at the new revision must NOT overwrite the
		// old revision's rows (separate revision → separate rows)
		if err := store.PersistResults(ctx, ComputeV1(d1)); err != nil {
			t.Fatal(err)
		}
		if err := store.PersistResults(ctx, ComputeV1(d2)); err != nil {
			t.Fatal(err)
		}
		var rows int
		if err := store.Pool.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.metric_result
			WHERE metric_key = 'protocol_compatibility'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 2 {
			t.Fatalf("protocol rows = %d, want 2 (one per revision — history preserved)", rows)
		}
	})
}

// R2-P2 回归：过期 Snapshot 在真实管道可达——binding 钉住的 snapshot 过期
// → 提取为 issue 行（此前仅 fixture 语义）。
func TestExpiredSnapshotReachableInPipeline(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		// healthy chain
		seedMetricChain(t, store, "cap-h", "prov-h", "bind-h", true)
		// expired-snapshot chain
		seedMetricChain(t, store, "cap-s", "prov-s", "bind-s", false)
		d, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var staleFound bool
		for _, r := range d.Rows {
			if r.Capability == "cap-s" {
				staleFound = true
				if r.ActiveBindings != 0 || r.BindingIssues != 1 {
					t.Fatalf("expired-snapshot row = %+v, want issues=1 active=0 (pipeline-reachable)", r)
				}
			}
		}
		if !staleFound {
			t.Fatal("expired-snapshot binding not extracted at all")
		}
	})
}

// R2-P3-1 回归：同一 provider 服务多个 capability 不再误报 duplicate。
func TestCrossCapabilityProviderNotDuplicate(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		// one provider serving two capabilities (legitimate)
		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO registry.resource_provider
				(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
			VALUES ('prov-multi', 'MODEL', 'svc://m', 'vendor-m', 'spiffe://saoaf.test/m', 'PUBLISHED', 1, 1)`); err != nil {
			t.Fatal(err)
		}
		var pid int
		if err := store.Pool.QueryRow(ctx,
			`SELECT id FROM registry.resource_provider WHERE provider_key='prov-multi'`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		var snapID int64
		if err := store.Pool.QueryRow(ctx, `
			INSERT INTO registry.provider_snapshot
				(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
				 profiles, generated_at, valid_until, state)
			VALUES ($1, 1, '2026.09', 'sha256:4444444444444444444444444444444444444444444444444444444444444444', 's',
			       'spiffe://saoaf.test/m', '[]'::jsonb, now(), now() + interval '1 day', 'PUBLISHED')
			RETURNING id`, pid).Scan(&snapID); err != nil {
			t.Fatal(err)
		}
		for ci, cap := range []string{"cap-m1", "cap-m2"} {
			if _, err := store.Pool.Exec(ctx, `
				INSERT INTO registry.capability_definition
					(capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
				VALUES ($1, 1, 1, 'MODEL', '{}', 'PUBLISHED', 'u')`, cap); err != nil {
				t.Fatal(err)
			}
			if _, err := store.Pool.Exec(ctx, `
				INSERT INTO registry.capability_binding
					(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
					 environment, scope, scope_hash, priority, state, revision, is_active)
				SELECT $1, cd.id, $2, $3, 'p', 'production', '{}'::jsonb, 'sha256:x', $5, 'PUBLISHED', 1, TRUE
				FROM registry.capability_definition cd WHERE cd.capability_key = $4`,
				"bind-"+cap, pid, snapID, cap, 100+ci); err != nil {
				t.Fatal(err)
			}
		}
		d, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range d.Rows {
			if r.DupProviderRows > 0 {
				t.Fatalf("cross-capability provider flagged as duplicate: %+v", r)
			}
		}
		// a TRUE duplicate: same (cap, provider) on two rows
		if _, err := store.Pool.Exec(ctx, `
			INSERT INTO registry.capability_binding
				(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
				 environment, scope, scope_hash, priority, state, revision, is_active)
			SELECT 'bind-m1-dup', cd.id, $1, $2, 'p', 'production', '{}'::jsonb, 'sha256:y', 200, 'PUBLISHED', 1, TRUE
			FROM registry.capability_definition cd WHERE cd.capability_key = 'cap-m1'`,
			pid, snapID); err != nil {
			t.Fatal(err)
		}
		d2, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var sawDup bool
		for _, r := range d2.Rows {
			if r.Capability == "cap-m1" && r.Provider == "prov-multi" && r.DupProviderRows > 0 {
				sawDup = true
			}
		}
		if !sawDup {
			t.Fatal("true same-capability duplicate not detected")
		}
	})
}

// seedMetricChain seeds a capability/provider/snapshot/binding chain; the
// snapshot is healthy or expired.
func seedMetricChain(t *testing.T, store Store, capKey, provKey, bindKey string, healthy bool) {
	t.Helper()
	ctx := context.Background()
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO registry.capability_definition
			(capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		VALUES ($1, 1, 1, 'MODEL', '{}', 'PUBLISHED', 'u')`, capKey); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO registry.resource_provider
			(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		VALUES ($1, 'MODEL', 'svc://x', 'vendor-r', 'spiffe://saoaf.test/x', 'PUBLISHED', 1, 1)`, provKey); err != nil {
		t.Fatal(err)
	}
	var pid int
	if err := store.Pool.QueryRow(ctx,
		`SELECT id FROM registry.resource_provider WHERE provider_key = $1`, provKey).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	validity := "now() + interval '1 day'"
	if !healthy {
		validity = "now() - interval '1 hour'"
	}
	var snapID int64
	if err := store.Pool.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO registry.provider_snapshot
			(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
			 profiles, generated_at, valid_until, state)
		VALUES (%d, 1, '2026.09', 'sha256:5555555555555555555555555555555555555555555555555555555555555555', 's',
		       'spiffe://saoaf.test/x', '[]'::jsonb, now() - interval '2 hour', %s, 'PUBLISHED')
		RETURNING id`, pid, validity)).Scan(&snapID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pool.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active)
		SELECT $1, cd.id, $2, $3, 'p', 'production', '{}'::jsonb, 'sha256:x', $5, 'PUBLISHED', 1, TRUE
		FROM registry.capability_definition cd WHERE cd.capability_key = $4`,
		bindKey, pid, snapID, capKey, 100+int(bindKey[len(bindKey)-1])); err != nil {
		t.Fatal(err)
	}
}

// R3-P1 回归（审查探针 TestR3SnapshotExpiryFingerprintBlindness 正名）：
// snapshot valid_until 越界是零写入的时钟事件——提取分类翻转（active→issue）
// 而指纹必须随之 bump，否则同 revision DO UPDATE 改写 OK 历史。
func TestSnapshotExpiryBumpsRevision(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		seedMetricChain(t, store, "cap-exp", "prov-exp", "bind-exp", true)
		d1, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		rev1 := d1.Revision

		// zero-write clock passage: expire the snapshot in place
		// (the freeze trigger allows valid_until changes)
		if _, err := store.Pool.Exec(ctx, `
			UPDATE registry.provider_snapshot
			SET valid_until = now() - interval '1 hour'
			WHERE provider_id = (SELECT id FROM registry.resource_provider WHERE provider_key = 'prov-exp')`); err != nil {
			t.Fatal(err)
		}
		d2, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d2.Revision == rev1 {
			t.Fatalf("snapshot expiry (clock passage) did NOT bump revision (%d) — history rewrite window", rev1)
		}
		// the classification flip must be visible in the extracted rows
		var flipped bool
		for _, r := range d2.Rows {
			if r.Capability == "cap-exp" && r.ActiveBindings == 0 && r.BindingIssues == 1 {
				flipped = true
			}
		}
		if !flipped {
			t.Fatal("expiry classification not reflected in extraction")
		}
		// both revisions' results coexist — history preserved
		if err := store.PersistResults(ctx, ComputeV1(d1)); err != nil {
			t.Fatal(err)
		}
		if err := store.PersistResults(ctx, ComputeV1(d2)); err != nil {
			t.Fatal(err)
		}
		var rows int
		_ = store.Pool.QueryRow(ctx, `
			SELECT count(DISTINCT dataset_revision) FROM saoaf.metric_result
			WHERE metric_key = 'protocol_compatibility'`).Scan(&rows)
		if rows != 2 {
			t.Fatalf("distinct protocol revisions = %d, want 2 (history preserved)", rows)
		}
	})
}

// R3-P1 回归（探针 TestR3SnapshotStateFlipFingerprintBlindness 正名）：
// snapshot state 原地翻转（freeze trigger 放行 state 变更）→ revision bump。
func TestSnapshotStateFlipBumpsRevision(t *testing.T) {
	withDBM(t, func(dsn string, store Store) {
		ctx := context.Background()
		seedMetricChain(t, store, "cap-flip", "prov-flip", "bind-flip", true)
		d1, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Pool.Exec(ctx, `
			UPDATE registry.provider_snapshot
			SET state = 'SUSPENDED'
			WHERE provider_id = (SELECT id FROM registry.resource_provider WHERE provider_key = 'prov-flip')`); err != nil {
			t.Fatal(err)
		}
		d2, err := store.ExtractDataset(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if d2.Revision == d1.Revision {
			t.Fatalf("snapshot state flip did NOT bump revision (%d)", d1.Revision)
		}
	})
}
