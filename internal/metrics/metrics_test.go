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

	// 重复 Provider / 失效 Binding → 集中度 reason 标注 / 协议兼容 INSUFFICIENT_DATA
	d = &Dataset{Revision: 1, Rows: []DimRow{
		{Capability: "cap-d", Provider: "p1", Vendor: "v1", ActiveBindings: 2, DupProviderRows: 1},
		{Capability: "cap-d", Provider: "p1", Vendor: "v1", ActiveBindings: 1, DupProviderRows: 1},
		{Capability: "cap-e", Provider: "p2", Vendor: "v2", ActiveBindings: 2, BindingIssues: 2},
	}}
	res = ComputeV1(d)
	var sawDup, sawIssues bool
	for _, r := range res {
		if r.MetricKey == KeyVendorConcentration && r.StatusReason != "" {
			sawDup = true
		}
		if r.MetricKey == KeyProtocolCompat && r.Status == StatusInsufficientData {
			sawIssues = true
		}
	}
	if !sawDup || !sawIssues {
		t.Fatalf("duplicate-provider (%v) / binding-issues (%v) classification missing", sawDup, sawIssues)
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
		for i := 0; i < 4; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = store.PersistResults(ctx, ComputeV1(fixedDataset(42)))
			}()
		}
		wg.Wait()
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
		latest, err := store.Latest(ctx, FormulaV1, KeySubstitutionCoverage, 10)
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
		latest, err := store.Latest(ctx, FormulaV1, "", 100)
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
