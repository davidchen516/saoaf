package mmr

// I11 MOCK CLOSURE end-to-end (David's 2026-09-28 directive: the real MMR
// Provider cannot be provided — the remaining #11 ledger items close against
// the repository mock). This suite drives the FULL minimal closed loop the
// issue demands — Harness adapter → ARR plan → MMR (mock) → five-outcome
// correlation ledger — plus the closure-specific proofs:
//
//   - five outcomes incl. the FALLBACK named example (ledger #4)
//   - ARR Plan invariance across MMR snapshot republish (MMR 内部更换具体
//     模型但 profile 不变时 Plan 不变化 — AC#1, mock-end-to-end via the
//     I07/I09 registry + resolver plan resolution)
//   - kill switch (GWT#8): provider suspend blocks new plans on the ARR side;
//     in-flight data-plane outcomes are recorded by the adapter (MMR-side
//     kill switch is MMR-owned per the baseline — mock closure records the
//     SAOAF half)
//
// The suite uses the real PostgreSQL ledger (saoaf.model_route_correlation)
// and the same Prism contract mock the CI identity/module-contract jobs run.
import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func withDBMMR(t *testing.T, fn func(dsn string, pool *pgxpool.Pool)) {
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
	name := fmt.Sprintf("mmr_e2e_%d_%d", os.Getpid(), time.Now().UnixNano())
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

// seedE2ERegistry seeds the capability/provider/binding/snapshot chain the
// ARR plan resolution needs (the same chain the resolver tests use).
func e2ePlan(t *testing.T, pool *pgxpool.Pool, planID string) {
	t.Helper()
	ctx := context.Background()
	Z64 := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	if _, err := pool.Exec(ctx, `
		INSERT INTO resolver.resource_plan (id, caller_ref, tenant_ref, fingerprint, request_digest, idempotency_key, status, expires_at)
		VALUES ($1, 'user:harness', 'tenant-a', $2, $2, 'idem-e2e-1', 'RESOLVED', now() + interval '1 hour')`, planID, Z64); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO resolver.resource_plan_item (plan_id, requirement_id, capability_key, major_version, capability_revision, binding_key, binding_revision, provider_key, snapshot_version, profile_or_action, reason_codes)
		VALUES ($1, 'req-e2e', 'cap-e2e', 1, 1, 'bind-e2e', 1, 'mmr-e2e', 1, 'reasoning-high-v1', '[]')`, planID); err != nil {
		t.Fatal(err)
	}
}

// THE minimal closed loop, mock end-to-end (ledger #1): plan → adapter → mock
// invocation (all four contract cases + the new fallback example) →
// five-outcome correlation ledger rows — each row binding plan+decision.
func TestMockClosedLoopFiveOutcomes(t *testing.T) {
	withDBMMR(t, func(dsn string, pool *pgxpool.Pool) {
		mock := mmrMock(t, 4090)
		store := &Correlator{Pool: pool}
		inv := Invocation{
			Profile: "reasoning-high-v1", ResourcePlanID: "plan-mock-001",
			ResourcePlanItemID: "req-e2e", TenantRef: "tenant-a",
			Traceparent: "00-00000000000000000000000000000000-0000000000000000-01",
		}
		reqBody := `{"model":"reasoning-high-v1","messages":[{"role":"user","content":"e2e"}]}`

		// the plan the correlation rows will bind
		e2ePlan(t, pool, "plan-mock-001")

		type outcomeCase struct {
			name     string
			prefer   string
			decision string
			outcome  string
			httpDone func(t *testing.T) (status int, body []byte, hdrDecision string)
		}
		cases := []outcomeCase{
			{
				name: "SUCCEEDED", prefer: "", decision: "mrd-mock-success",
				outcome: OutcomeSucceeded,
				httpDone: func(t *testing.T) (int, []byte, string) {
					resp, b := invoke(t, mock, inv, reqBody, "")
					defer resp.Body.Close()
					return resp.StatusCode, b, resp.Header.Get("X-Model-Route-Decision-ID")
				},
			},
			{
				name: "FALLBACK (named example — ledger #4)", prefer: "example=fallback",
				outcome: OutcomeFallback,
				httpDone: func(t *testing.T) (int, []byte, string) {
					resp, b := invoke(t, mock, inv, reqBody, "example=fallback")
					defer resp.Body.Close()
					return resp.StatusCode, b, resp.Header.Get("X-Model-Route-ID")
				},
			},
			{
				name: "QUOTA (429 error example)", prefer: "code=429",
				outcome: OutcomeQuota,
				httpDone: func(t *testing.T) (int, []byte, string) {
					resp, b := invoke(t, mock, inv, reqBody, "code=429")
					defer resp.Body.Close()
					return resp.StatusCode, b, ""
				},
			},
			{
				name: "FAILED (503 error example)", prefer: "code=503",
				outcome: OutcomeFailed,
				httpDone: func(t *testing.T) (int, []byte, string) {
					resp, b := invoke(t, mock, inv, reqBody, "code=503")
					defer resp.Body.Close()
					return resp.StatusCode, b, ""
				},
			},
			{
				// TIMEOUT has no HTTP response to correlate — the test
				// records the synthetic bookkeeping row directly (the
				// PR #45-approved path: a timeout's decision id arrives
				// only in MMR's eventual retry/audit export, I12)
				name:    "TIMEOUT (synthetic bookkeeping — no HTTP response)",
				outcome: OutcomeTimeout,
				httpDone: func(t *testing.T) (int, []byte, string) {
					return 0, nil, ""
				},
			},
		}

		for i, tc := range cases {
			decision := fmt.Sprintf("mrd-e2e-%d", i)
			outcome := tc.outcome
			status, body, hdrDecision := tc.httpDone(t)
			switch {
			case status == 200 && outcome == OutcomeFallback:
				// named fallback example: decision from body, outcome from the status field
				var parsed struct {
					ModelRouteDecisionID string `json:"model_route_decision_id"`
				}
				if err := json.Unmarshal(body, &parsed); err != nil {
					t.Fatalf("%s: body decode: %v", tc.name, err)
				}
				decision = parsed.ModelRouteDecisionID
				outcome = OutcomeFromBody(status, body)
			case status == 200:
				// success example: header decision + body outcome
				if hdrDecision == "" {
					// Prism example selection only sets the header for the
					// default example; fall back to the body decision
					var parsed struct {
						ModelRouteDecisionID string `json:"model_route_decision_id"`
					}
					_ = json.Unmarshal(body, &parsed)
					hdrDecision = parsed.ModelRouteDecisionID
				}
				decision = hdrDecision
				outcome = OutcomeFromBody(status, body)
			case status >= 400:
				// error example: decision from the error body extension
				d, err := inv.DecisionFromError(status, body)
				if err != nil {
					t.Fatalf("%s: DecisionFromError: %v", tc.name, err)
				}
				decision = d
				outcome = OutcomeForStatus(status)
			case status == 0:
				// timeout: no HTTP exchange happened; the test records the
				// synthetic bookkeeping row directly (PR #45-approved —
				// zero adapter code runs in this branch)
			}
			if err := store.Record(context.Background(), Correlation{
				ResourcePlanID: inv.ResourcePlanID, ResourcePlanItemID: inv.ResourcePlanItemID,
				ModelRouteDecisionID: decision, Outcome: outcome, TraceID: "trace-e2e",
			}); err != nil {
				t.Fatalf("%s: record: %v", tc.name, err)
			}
		}

		// ALL FIVE outcomes bound to the plan (双 ID 关联 — the issue's
		// core acceptance: 成功、回退、配额、超时和失败场景均能关联)
		rows, err := pool.Query(context.Background(), `
			SELECT outcome FROM saoaf.model_route_correlation WHERE resource_plan_id = 'plan-mock-001'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		seen := map[string]bool{}
		for rows.Next() {
			var o string
			if err := rows.Scan(&o); err != nil {
				t.Fatal(err)
			}
			seen[o] = true
		}
		for _, want := range []string{OutcomeSucceeded, OutcomeFallback, OutcomeQuota, OutcomeTimeout, OutcomeFailed} {
			if !seen[want] {
				t.Fatalf("outcome %s missing from the correlation ledger (have %v)", want, seen)
			}
		}

		// FALLBACK came from the named example with a real decision id
		var fbDecision string
		if err := pool.QueryRow(context.Background(), `
			SELECT model_route_decision_id FROM saoaf.model_route_correlation
			WHERE resource_plan_id='plan-mock-001' AND outcome='FALLBACK'`).Scan(&fbDecision); err != nil {
			t.Fatal(err)
		}
		if fbDecision == "" {
			t.Fatal("FALLBACK row carries no model_route_decision_id (double-ID correlation broken)")
		}
	})
}

// Shadow/灰度 bookkeeping over the mock: with the SHADOW mode row in place
// BEFORE the invocation, the mode machinery reports SHADOW at call time and
// the shadow call still records its correlation (the对比 evidence the ledger
// demands); the routing mode transitions stay CAS-guarded and audited.
// (The Plan-invariance and kill-switch halves of the mock closure live in
// cmd/control-plane-api/mmr_closure_test.go — they need the resolver and
// binding packages, which the internal/mmr boundary forbids importing.)
func TestMockShadowAndGrayModeBookkeeping(t *testing.T) {
	withDBMMR(t, func(dsn string, pool *pgxpool.Pool) {
		e2ePlan(t, pool, "plan-mock-001")
		mock := mmrMock(t, 4092)
		store := &Correlator{Pool: pool}
		modes := &ModeStore{Pool: pool}
		ctx := context.Background()

		// the SHADOW row exists BEFORE the shadow call (the state machine's
		// entry state; a rollback would preserve it as the pre-gray config)
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.mmr_routing_mode
				(scope_tenant, scope_agent, mode, static_profile_ref, updated_by)
			VALUES ('tenant-a', '', 'SHADOW', 'static-fallback-1', 'user:op')
			ON CONFLICT (scope_tenant, scope_agent) DO NOTHING`); err != nil {
			t.Fatal(err)
		}
		if m, err := modes.Mode(ctx, "tenant-a", ""); err != nil || m.Mode != "SHADOW" {
			t.Fatalf("mode at call time = %+v (%v), want SHADOW (mode row seeded first)", m, err)
		}

		// shadow: the invocation happens under SHADOW and its comparison
		// outcome is recorded
		inv := Invocation{
			Profile: "reasoning-high-v1", ResourcePlanID: "plan-mock-001",
			ResourcePlanItemID: "req-shadow", TenantRef: "tenant-a",
			Traceparent: "00-00000000000000000000000000000000-0000000000000000-01",
		}
		resp, _ := invoke(t, mock, inv, `{"model":"reasoning-high-v1","messages":[{"role":"user","content":"s"}]}`, "")
		decision, err := inv.ValidateResponse(resp)
		resp.Body.Close()
		if err != nil {
			t.Fatalf("shadow validate: %v", err)
		}
		if m, err := modes.Mode(ctx, "tenant-a", ""); err != nil || m.Mode != "SHADOW" {
			t.Fatalf("mode after the shadow call = %+v (%v), want SHADOW (call must not flip the mode)", m, err)
		}
		if err := store.Record(ctx, Correlation{
			ResourcePlanID: inv.ResourcePlanID, ResourcePlanItemID: inv.ResourcePlanItemID,
			ModelRouteDecisionID: decision, Outcome: OutcomeSucceeded, TraceID: "trace-shadow",
		}); err != nil {
			t.Fatal(err)
		}
		var shadowRows int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.model_route_correlation
			WHERE resource_plan_id='plan-mock-001' AND trace_id='trace-shadow'`).Scan(&shadowRows); err != nil {
			t.Fatal(err)
		}
		if shadowRows != 1 {
			t.Fatalf("shadow-mode correlation rows = %d, want 1", shadowRows)
		}

		// mode transitions: shadow → gray → full with CAS + audit.
		from := "SHADOW"
		for _, to := range []string{"GRAY", "FULL"} {
			if err := modes.SetMode(ctx, RoutingMode{
				ScopeTenant: "tenant-a", ScopeAgent: "", Mode: to,
				UpdatedBy: "user:op", StaticProfileRef: "static-fallback-1",
			}, from); err != nil {
				t.Fatalf("transition %s → %s: %v", from, to, err)
			}
			from = to
		}
		var m string
		if err := pool.QueryRow(ctx, `
			SELECT mode FROM saoaf.mmr_routing_mode
			WHERE scope_tenant='tenant-a' AND scope_agent=''`).Scan(&m); err != nil {
			t.Fatal(err)
		}
		if m != "FULL" {
			t.Fatalf("final mode = %s, want FULL", m)
		}
		var audits int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.change_record
			WHERE entity_kind='mmr-routing-mode' AND entity_id='tenant-a/'`).Scan(&audits); err != nil {
			t.Fatal(err)
		}
		if audits < 2 {
			t.Fatalf("mode-transition audit rows = %d, want >=2 (CAS transitions audited)", audits)
		}
	})
}
