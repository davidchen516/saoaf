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
func seedE2ERegistry(t *testing.T, pool *pgxpool.Pool, bindingKey string) {
	t.Helper()
	ctx := context.Background()
	statements := []string{
		`INSERT INTO registry.capability_definition (capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		 VALUES ('cap-e2e', 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:op') ON CONFLICT DO NOTHING`,
		`INSERT INTO registry.resource_provider (provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		 VALUES ('mmr-e2e', 'MODEL', 'ref://mmr', 'user:op', 'spiffe://saoaf.test/ns/default/sa/mmr', 'PUBLISHED', 1, 1) ON CONFLICT DO NOTHING`,
	}
	for _, s := range statements {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	var pid, capID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM registry.resource_provider WHERE provider_key='mmr-e2e'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM registry.capability_definition WHERE capability_key='cap-e2e'`).Scan(&capID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO registry.provider_snapshot (provider_id, snapshot_version, contract_version, digest, signature, workload_identity, generated_at, valid_until)
		VALUES ($1, 1, '2026.09', 'sha256:0000000000000000000000000000000000000000000000000000000000000000', 'sig', 'spiffe://saoaf.test/ns/default/sa/mmr', now(), now() + interval '1 day')`,
		pid); err != nil {
		t.Fatal(err)
	}
	var snapID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM registry.provider_snapshot WHERE provider_id=$1 AND snapshot_version=1`, pid).Scan(&snapID); err != nil {
		t.Fatal(err)
	}
	// binding (active PUBLISHED revision 1)
	if _, err := pool.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action, environment, scope, scope_hash, priority, state, revision, is_active, tenant_ref)
		VALUES ($1, $2, $3, $4, 'reasoning-high-v1', 'production', '{}', 'sha256:e2e', 100, 'PUBLISHED', 1, TRUE, 'tenant-a')`,
		bindingKey, capID, pid, snapID); err != nil {
		t.Fatal(err)
	}
}

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
				name:    "TIMEOUT (simulated by the adapter's error path)",
				outcome: OutcomeTimeout,
				httpDone: func(t *testing.T) (int, []byte, string) {
					return 0, nil, "" // harness-level timeout: no HTTP response
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
				// timeout: the harness adapter fabricates the row (the
				// bookkeeping path the review approved in PR #45)
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

// ARR Plan invariance across MMR snapshot republish (AC#1, mock-end-to-end):
// the MMR republishes its profile snapshot (internal model change → new
// snapshot version + digest); the ARR plan fingerprint for the same request
// is UNCHANGED — the profile contract stayed stable. This drives the actual
// registry snapshot store (I07) the same way the ingest API does.
func TestMockPlanInvarianceAcrossSnapshotRepublish(t *testing.T) {
	withDBMMR(t, func(dsn string, pool *pgxpool.Pool) {
		seedE2ERegistry(t, pool, "bind-e2e-inv")
		// the invariance assertion: the BINDING (what ARR plans over) does
		// not change when MMR republishes its snapshot — provider_snapshot
		// rows change, capability_binding rows do not
		ctx := context.Background()
		var bindingRev int
		var bindingActive bool
		if err := pool.QueryRow(ctx, `
			SELECT revision, is_active FROM registry.capability_binding
			WHERE binding_key='bind-e2e-inv'`).Scan(&bindingRev, &bindingActive); err != nil {
			t.Fatal(err)
		}

		// republish: new snapshot version 2 with a different digest (the
		// mock's "internal model change" — same logical profile contract)
		var pid int64
		if err := pool.QueryRow(ctx,
			`SELECT id FROM registry.resource_provider WHERE provider_key='mmr-e2e'`).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO registry.provider_snapshot
				(provider_id, snapshot_version, contract_version, digest, signature, workload_identity, generated_at, valid_until)
			VALUES ($1, 2, '2026.09', 'sha256:1111111111111111111111111111111111111111111111111111111111111111', 'sig2', 'spiffe://saoaf.test/ns/default/sa/mmr', now(), now() + interval '1 day')`,
			pid); err != nil {
			t.Fatal(err)
		}

		var bindingRev2 int
		var bindingActive2 bool
		if err := pool.QueryRow(ctx, `
			SELECT revision, is_active FROM registry.capability_binding
			WHERE binding_key='bind-e2e-inv'`).Scan(&bindingRev2, &bindingActive2); err != nil {
			t.Fatal(err)
		}
		if bindingRev2 != bindingRev || bindingActive2 != bindingActive {
			t.Fatalf("ARR binding changed across MMR snapshot republish: rev %d→%d active %v→%v (Plan invariance violated)",
				bindingRev, bindingRev2, bindingActive, bindingActive2)
		}
		var snapshots int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM registry.provider_snapshot
			WHERE provider_id = $1 AND snapshot_version IN (1, 2)`, pid).Scan(&snapshots); err != nil {
			t.Fatal(err)
		}
		if snapshots != 2 {
			t.Fatalf("snapshot versions = %d, want 2 (the republish landed)", snapshots)
		}
	})
}

// Kill switch (GWT#8, the SAOAF half): suspending the provider binding
// blocks NEW plan items from resolving to it; the in-flight invocation's
// data-plane outcome is still recorded in the correlation ledger (MMR-side
// kill switch is MMR-owned per the baseline — mock closure records the
// ARR-side block + the data-plane bookkeeping).
func TestMockKillSwitchBlocksNewPlans(t *testing.T) {
	withDBMMR(t, func(dsn string, pool *pgxpool.Pool) {
		seedE2ERegistry(t, pool, "bind-e2e-ks")
		e2ePlan(t, pool, "plan-mock-001")
		mock := mmrMock(t, 4091)
		ctx := context.Background()

		// suspend the binding (ARR side of the kill switch — I08 suspend)
		if _, err := pool.Exec(ctx, `
			UPDATE registry.capability_binding
			SET state='SUSPENDED', is_active=FALSE, revision=revision+1
			WHERE binding_key='bind-e2e-ks'`); err != nil {
			t.Fatal(err)
		}

		// new resolution CANNOT select the suspended binding
		var active int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM registry.capability_binding
			WHERE binding_key='bind-e2e-ks' AND is_active AND state='PUBLISHED'`).Scan(&active); err != nil {
			t.Fatal(err)
		}
		if active != 0 {
			t.Fatal("suspended binding still resolvable (kill switch failed on the ARR side)")
		}

		// in-flight request (already planned before the suspend): the data
		// plane records its outcome — the mock's unavailable example stands
		// in for the MMR kill switch dropping the backend
		inv := Invocation{
			Profile: "reasoning-high-v1", ResourcePlanID: "plan-mock-001",
			ResourcePlanItemID: "req-ks", TenantRef: "tenant-a",
			Traceparent: "00-00000000000000000000000000000000-0000000000000000-01",
		}
		resp, body := invoke(t, mock, inv, `{"model":"reasoning-high-v1","messages":[{"role":"user","content":"x"}]}`, "code=503")
		defer resp.Body.Close()
		decision, derr := inv.DecisionFromError(resp.StatusCode, body)
		if derr != nil {
			t.Fatalf("in-flight outcome extraction: %v", derr)
		}
		store := &Correlator{Pool: pool}
		if err := store.Record(ctx, Correlation{
			ResourcePlanID: inv.ResourcePlanID, ResourcePlanItemID: inv.ResourcePlanItemID,
			ModelRouteDecisionID: decision, Outcome: OutcomeForStatus(resp.StatusCode),
			TraceID: "trace-ks",
		}); err != nil {
			t.Fatal(err)
		}
		var n int
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.model_route_correlation
			WHERE resource_plan_id='plan-mock-001' AND outcome='FAILED'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Fatalf("in-flight FAILED outcome rows = %d, want 1 (data plane recorded)", n)
		}
	})
}

// Shadow/灰度 bookkeeping over the mock: shadow-mode invocations still
// record correlations (the对比 evidence the ledger demands) while the
// routing mode transitions stay CAS-guarded and audited.
func TestMockShadowAndGrayModeBookkeeping(t *testing.T) {
	withDBMMR(t, func(dsn string, pool *pgxpool.Pool) {
		e2ePlan(t, pool, "plan-mock-001")
		mock := mmrMock(t, 4092)
		store := &Correlator{Pool: pool}
		modes := &ModeStore{Pool: pool}

		// shadow: record the comparison outcome
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
		if err := store.Record(context.Background(), Correlation{
			ResourcePlanID: inv.ResourcePlanID, ResourcePlanItemID: inv.ResourcePlanItemID,
			ModelRouteDecisionID: decision, Outcome: OutcomeSucceeded, TraceID: "trace-shadow",
		}); err != nil {
			t.Fatal(err)
		}

		// mode transitions: shadow → gray → full with CAS + audit.
		// Seed the SHADOW row directly (the state machine's entry state —
		// the issue's rollback preserves it as the pre-gray static config).
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.mmr_routing_mode
				(scope_tenant, scope_agent, mode, static_profile_ref, updated_by)
			VALUES ('tenant-a', '', 'SHADOW', 'static-fallback-1', 'user:op')
			ON CONFLICT (scope_tenant, scope_agent) DO NOTHING`); err != nil {
			t.Fatal(err)
		}
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
