package mmr

// Unit tests: invocation contract, response validation, the 灰度 state
// machine, and the correlation ledger (real PG). Mock-driven end-to-end
// tests live in mock_test.go; the forbid-field regression in forbid_test.go.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func withDBM(t *testing.T, fn func(dsn string, pool *pgxpool.Pool)) {
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
	name := fmt.Sprintf("mmr_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	fn(dsn, pool)
}

func inv() Invocation {
	// valid W3C traceparent (the MMR contract validates the pattern)
	return Invocation{
		Profile: "reasoning-high-v1", ResourcePlanID: "plan-1",
		ResourcePlanItemID: "item-1", TenantRef: "tenant-a",
		Traceparent: "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01",
	}
}

// 运行时契约头（spec §4 + mock contract）：关联双 ID + 租户 + traceparent。
func TestInvocationHeaders(t *testing.T) {
	h := inv().Headers()
	if h.Get("X-Resource-Plan-ID") != "plan-1" ||
		h.Get("X-Resource-Plan-Item-ID") != "item-1" ||
		h.Get("X-Tenant-Ref") != "tenant-a" ||
		h.Get("traceparent") != "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" {
		t.Fatalf("headers = %v", h)
	}
	// no model payload on the invocation — requests live in the Harness
	if inv().Profile == "" {
		t.Fatal("profile required")
	}
}

// 响应校验：decision id 必在；echo 的 plan id 必须一致（透传一致性）。
func TestValidateResponse(t *testing.T) {
	ok := func() *http.Response {
		r := http.Response{Header: http.Header{}}
		r.Header.Set("X-Model-Route-Decision-ID", "mrd-1")
		r.Header.Set("X-Resource-Plan-ID", "plan-1")
		return &r
	}
	if _, err := inv().ValidateResponse(ok()); err != nil {
		t.Fatalf("valid response rejected: %v", err)
	}
	bad := ok()
	bad.Header.Del("X-Model-Route-Decision-ID")
	if _, err := inv().ValidateResponse(bad); err != ErrMissingDecisionID {
		t.Fatalf("missing decision: %v", err)
	}
	mismatch := ok()
	mismatch.Header.Set("X-Resource-Plan-ID", "plan-other")
	if _, err := inv().ValidateResponse(mismatch); err == nil {
		t.Fatal("plan echo mismatch accepted")
	}
}

// 五结局错误信封：decision id 在错误体内也要可提取（specs §5）。
func TestDecisionFromError(t *testing.T) {
	id, err := inv().DecisionFromError(429, []byte(
		`{"error":{"code":"QUOTA_EXCEEDED"},"model_route_decision_id":"mrd-q","resource_plan_id":"plan-1"}`))
	if err != nil || id != "mrd-q" {
		t.Fatalf("quota decision: %q %v", id, err)
	}
	if _, err := inv().DecisionFromError(400, []byte(
		`{"error":{"code":"PROFILE_NOT_FOUND"}}`)); err != ErrMissingDecisionID {
		t.Fatalf("profile-not-found: %v", err)
	}
	if _, err := inv().DecisionFromError(429, []byte(
		`{"model_route_decision_id":"mrd-x","resource_plan_id":"plan-other"}`)); err == nil {
		t.Fatal("plan mismatch in error envelope accepted")
	}
	if OutcomeForStatus(200) != OutcomeSucceeded || OutcomeForStatus(429) != OutcomeQuota ||
		OutcomeForStatus(503) != OutcomeFailed {
		t.Fatal("outcome mapping broken")
	}
}

// 灰度状态机：shadow → 灰度 → 全量；任意态可回退；回退可重启灰度。
func TestRoutingModeTransitions(t *testing.T) {
	legal := [][2]string{
		{"", "SHADOW"}, {"", "GRAY"}, {"", "FULL"}, // initial writes (audited)
		{"SHADOW", "GRAY"}, {"GRAY", "FULL"},
		{"SHADOW", "ROLLED_BACK"}, {"GRAY", "ROLLED_BACK"}, {"FULL", "ROLLED_BACK"},
		{"ROLLED_BACK", "GRAY"},
	}
	for _, tr := range legal {
		if !ValidateModeTransition(tr[0], tr[1]) {
			t.Fatalf("legal transition rejected: %v", tr)
		}
	}
	illegal := [][2]string{
		{"SHADOW", "FULL"}, {"FULL", "GRAY"},
		{"FULL", "SHADOW"}, {"ROLLED_BACK", "FULL"},
	}
	for _, tr := range illegal {
		if ValidateModeTransition(tr[0], tr[1]) {
			t.Fatalf("illegal transition allowed: %v", tr)
		}
	}
}

// 关联台账（真实 PG）：五结局各一行；重放幂等吸收。
func TestCorrelationFiveOutcomes(t *testing.T) {
	withDBM(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		c := Correlator{Pool: pool}
		outcomes := []string{OutcomeSucceeded, OutcomeFallback, OutcomeQuota, OutcomeTimeout, OutcomeFailed}
		for i, o := range outcomes {
			if err := c.Record(ctx, Correlation{
				ResourcePlanID:       "plan-1",
				ResourcePlanItemID:   fmt.Sprintf("item-%d", i),
				ModelRouteDecisionID: fmt.Sprintf("mrd-%d", i),
				Outcome:              o,
				UsageRef:             "usage-1",
				TraceID:              "trace-1",
			}); err != nil {
				t.Fatalf("record %s: %v", o, err)
			}
		}
		// replay the same (item, decision) — absorbed
		if err := c.Record(ctx, Correlation{
			ResourcePlanID: "plan-1", ResourcePlanItemID: "item-0",
			ModelRouteDecisionID: "mrd-0", Outcome: OutcomeSucceeded,
		}); err != nil {
			t.Fatal(err)
		}
		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.model_route_correlation WHERE resource_plan_id = 'plan-1'`).Scan(&rows); err != nil {
			t.Fatal(err)
		}
		if rows != 5 {
			t.Fatalf("rows = %d, want 5 (五结局)", rows)
		}
		// 双 ID 关联完整性：每行两 ID 齐备（列 NOT NULL 保证，负向由 DB CHECK 承载）
		if err := c.Record(ctx, Correlation{Outcome: OutcomeSucceeded}); err == nil {
			t.Fatal("correlation without IDs accepted")
		}
	})
}

// 灰度模式的存储语义（真实 PG）：作用域覆盖（Agent > 租户 > 全局）+ CAS。
func TestModeStoreScopingAndCAS(t *testing.T) {
	withDBM(t, func(dsn string, pool *pgxpool.Pool) {
		ctx := context.Background()
		s := ModeStore{Pool: pool}
		// global → shadow
		if err := s.SetMode(ctx, RoutingMode{Mode: "SHADOW", UpdatedBy: "user:ops"}, ""); err != nil {
			t.Fatal(err)
		}
		// tenant overrides global
		if err := s.SetMode(ctx, RoutingMode{ScopeTenant: "tenant-a", Mode: "GRAY", UpdatedBy: "user:ops"}, ""); err != nil {
			t.Fatal(err)
		}
		// agent overrides tenant
		if err := s.SetMode(ctx, RoutingMode{ScopeTenant: "tenant-a", ScopeAgent: "agent-1", Mode: "FULL", UpdatedBy: "user:ops"}, ""); err != nil {
			t.Fatal(err)
		}
		m, err := s.Mode(ctx, "tenant-a", "agent-1")
		if err != nil || m == nil || m.Mode != "FULL" {
			t.Fatalf("agent scope: %+v %v", m, err)
		}
		m, err = s.Mode(ctx, "tenant-a", "agent-2")
		if err != nil || m == nil || m.Mode != "GRAY" {
			t.Fatalf("tenant scope: %+v %v", m, err)
		}
		m, err = s.Mode(ctx, "tenant-b", "agent-9")
		if err != nil || m == nil || m.Mode != "SHADOW" {
			t.Fatalf("global scope: %+v %v", m, err)
		}
		// CAS: stale expected value loses
		if err := s.SetMode(ctx, RoutingMode{ScopeTenant: "tenant-a", ScopeAgent: "agent-1", Mode: "ROLLED_BACK", UpdatedBy: "user:ops"}, "SHADOW"); err == nil {
			t.Fatal("stale CAS accepted")
		}
		// legal rollback from FULL
		if err := s.SetMode(ctx, RoutingMode{ScopeTenant: "tenant-a", ScopeAgent: "agent-1", Mode: "ROLLED_BACK", StaticProfileRef: "static/legacy.yaml", UpdatedBy: "user:ops"}, "FULL"); err != nil {
			t.Fatal(err)
		}
		m, _ = s.Mode(ctx, "tenant-a", "agent-1")
		if m.Mode != "ROLLED_BACK" || m.StaticProfileRef == "" {
			t.Fatalf("rollback state: %+v", m)
		}
		// audit trail written (历史不删——经 change_record)
		var audits int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM saoaf.change_record WHERE entity_kind = 'mmr-routing-mode'`).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		if audits != 4 {
			t.Fatalf("mode audit rows = %d, want 4", audits)
		}
	})
}
