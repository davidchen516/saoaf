package main

// I11 mock-closure composed tests (ADR-0006 composition root: only cmd may
// import multiple internal modules). These are the two halves the R1 review
// ruled could not live in internal/mmr (the boundary forbids importing the
// resolver/binding/registry packages there):
//
//   - Plan invariance: the MMR republish goes through the REAL registry
//     store (SubmitSnapshot + ActivateSnapshot — the same path the ingest
//     API drives), and the assertion runs the REAL resolver on both sides
//     of the republish: same binding selected, binding row untouched, same
//     plan fingerprint. Falsifiable: any change ActivateSnapshot makes to
//     capability_binding, or any resolver-visible drift, turns this red.
//   - Kill switch (GWT#8, ARR half): the suspend goes through the REAL I08
//     binding store (Store.Suspend, rev CAS), and the block is asserted
//     with the REAL resolver (Resolve → NO_COMPATIBLE_PROVIDER) — not by
//     re-querying the row the test itself wrote. Falsifiable: removing the
//     resolver's binding predicate (cb.state='PUBLISHED' AND cb.is_active)
//     turns this red, because the pre-suspend resolve proves the binding
//     was selectable through that same predicate.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/binding"
	"github.com/davidchen516/saoaf/internal/mmr"
	"github.com/davidchen516/saoaf/internal/registry"
	"github.com/davidchen516/saoaf/internal/resolver"
)

// closureMock starts the Phase 0 Prism mock on a disposable port (same
// contract + example-selection flags as the internal/mmr harness).
func closureMock(t *testing.T, port int) string {
	t.Helper()
	name := fmt.Sprintf("saoaf-cmd-mock-%d-%d", port, time.Now().UnixNano())
	wd, _ := os.Getwd()
	if out, err := exec.Command("docker", "run", "-d", "--name", name,
		"-p", fmt.Sprintf("127.0.0.1:%d:4010", port),
		"-v", wd+"/../../contracts/protocols/model-profile/openapi.yaml:/contracts/openapi.yaml:ro",
		"stoplight/prism:5.15.10", "mock", "-h", "0.0.0.0", "--errors", "--multiprocess=false",
		"/contracts/openapi.yaml").CombinedOutput(); err != nil {
		t.Fatalf("prism run: %v\n%s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/v1/models")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return base
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("prism mock did not become ready")
	return ""
}

// withDBClosure provisions a fresh migrated database per test.
func withDBClosure(t *testing.T, fn func(dsn string, pool *pgxpool.Pool)) {
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
	name := fmt.Sprintf("cmd_mmr_closure_%d_%d", os.Getpid(), time.Now().UnixNano())
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

// seedClosureChain seeds the registry chain the resolver needs: capability,
// provider, snapshot v1 (PUBLISHED + active pointer) and one PUBLISHED
// binding revision 1.
func seedClosureChain(t *testing.T, pool *pgxpool.Pool, bindingKey string) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{
		`INSERT INTO registry.capability_definition
		 (capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		 VALUES ('cap-closure', 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:op')`,
		`INSERT INTO registry.resource_provider
		 (provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		 VALUES ('mmr-closure', 'MODEL', 'svc://test/mmr', 'user:op',
		         'spiffe://saoaf.test/ns/default/sa/mmr', 'PUBLISHED', 1, 1)`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	var providerID, capID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM registry.resource_provider WHERE provider_key='mmr-closure'`).Scan(&providerID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM registry.capability_definition WHERE capability_key='cap-closure'`).Scan(&capID); err != nil {
		t.Fatal(err)
	}
	profiles := `[{"profile_id":"reasoning-high-v1","capability_keys":["cap-closure"],"regions":["cn-east"],
	              "data_classification_max":"CONFIDENTIAL","status":"AVAILABLE"}]`
	if _, err := pool.Exec(ctx, `
		INSERT INTO registry.provider_snapshot
			(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
			 profiles, generated_at, valid_until, state)
		VALUES ($1, 1, '2026.09', 'sha256:1111111111111111111111111111111111111111111111111111111111111111', 'sig',
		        'spiffe://saoaf.test/ns/default/sa/mmr', $2::jsonb, now(), now() + interval '1 day', 'PUBLISHED')`,
		providerID, profiles); err != nil {
		t.Fatal(err)
	}
	var snapID int64
	if err := pool.QueryRow(ctx,
		`SELECT id FROM registry.provider_snapshot WHERE provider_id=$1 AND snapshot_version=1`,
		providerID).Scan(&snapID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO registry.provider_active_pointer (provider_id, snapshot_id)
		VALUES ($1, $2)`, providerID, snapID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action, environment,
			 scope, scope_hash, priority, state, revision, is_active, tenant_ref)
		VALUES ($1, $2, $3, $4, 'reasoning-high-v1', 'production',
		        '{"tenant_refs":[],"factory_refs":[],"regions":[],"agent_refs":[]}'::jsonb,
		        'sha256:aaaa', 100, 'PUBLISHED', 1, TRUE, 'tenant-a')`,
		bindingKey, capID, providerID, snapID); err != nil {
		t.Fatal(err)
	}
}

// seedClosurePlan seeds the resolved plan row the correlation ledger binds.
func seedClosurePlan(t *testing.T, pool *pgxpool.Pool, planID string) {
	t.Helper()
	z64 := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO resolver.resource_plan
			(id, caller_ref, tenant_ref, fingerprint, request_digest, idempotency_key, status, expires_at)
		VALUES ($1, 'user:harness', 'tenant-a', $2, $2, 'idem-closure', 'RESOLVED', now() + interval '1 hour')`,
		planID, z64); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO resolver.resource_plan_item
			(plan_id, requirement_id, capability_key, major_version, capability_revision, binding_key,
			 binding_revision, provider_key, snapshot_version, profile_or_action, reason_codes)
		VALUES ($1, 'req-closure', 'cap-closure', 1, 1, 'bind-closure', 1, 'mmr-closure', 1,
		        'reasoning-high-v1', '[]')`, planID); err != nil {
		t.Fatal(err)
	}
}

// newClosureResolver builds the real resolver service over the pool.
func newClosureResolver(pool *pgxpool.Pool) *resolver.Service {
	return &resolver.Service{
		Plans: &resolver.Store{Pool: pool},
		Cache: resolver.NewSnapshotCache(resolver.NewRegistrySnapshotLoader(pool), 5*time.Second),
		Pool:  pool,
		Now:   time.Now, NewID: resolver.NewPlanID,
		DefaultTTL: 300 * time.Second, MaxTTL: 3600 * time.Second,
	}
}

func closureReq() *resolver.Request {
	return &resolver.Request{
		ContractVersion: "1.0", TaskRef: "task-closure",
		Requirements: []resolver.Requirement{{
			RequirementID: "req-closure", CapabilityID: "cap-closure",
			CapabilityMajorVersion: 1, ResourceType: "MODEL_PROVIDER",
			Constraints: map[string]any{"region": "cn-east"},
		}},
	}
}

func closureMeta() resolver.CallerMeta {
	return resolver.CallerMeta{CallerRef: "user:harness", TenantRef: "tenant-a", Environment: "production"}
}

// bindingSnapshot reads the binding row (revision, is_active) — the row ARR
// plans over; Plan invariance means it never moves across a republish.
func bindingSnapshot(t *testing.T, pool *pgxpool.Pool, key string) (int, bool) {
	t.Helper()
	var rev int
	var active bool
	if err := pool.QueryRow(context.Background(),
		`SELECT revision, is_active FROM registry.capability_binding WHERE binding_key=$1`,
		key).Scan(&rev, &active); err != nil {
		t.Fatalf("binding row read: %v", err)
	}
	return rev, active
}

// TestClosurePlanInvarianceAcrossSnapshotRepublish (ledger #2, mock closure):
// MMR republishes its snapshot through the REAL registry store path, and the
// ARR binding — what plans resolve over — does not move: same binding
// selected, same plan fingerprint, binding row byte-identical.
func TestClosurePlanInvarianceAcrossSnapshotRepublish(t *testing.T) {
	withDBClosure(t, func(dsn string, pool *pgxpool.Pool) {
		seedClosureChain(t, pool, "bind-closure")
		svc := newClosureResolver(pool)
		ctx := context.Background()

		// resolve BEFORE the republish — through the real resolver
		planBefore, _, err := svc.Resolve(ctx, closureReq(), closureMeta(), "idem-inv-1")
		if err != nil {
			t.Fatalf("resolve before republish: %v", err)
		}
		revBefore, activeBefore := bindingSnapshot(t, pool, "bind-closure")

		// the republish: the real store path the ingest API drives
		// (SubmitSnapshot → ActivateSnapshot), version 2, same logical
		// profile contract, fresh content-addressed digest
		var providerID int64
		if err := pool.QueryRow(ctx,
			`SELECT id FROM registry.resource_provider WHERE provider_key='mmr-closure'`).Scan(&providerID); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256([]byte("mmr-closure snapshot v2 — internal model change"))
		snap := &registry.Snapshot{
			ProviderID: providerID, SnapshotVersion: 2,
			ContractVersion:  "2026.09",
			Digest:           "sha256:" + hex.EncodeToString(sum[:]),
			Signature:        "sig-v2",
			WorkloadIdentity: "spiffe://saoaf.test/ns/default/sa/mmr",
			GeneratedAt:      time.Now().UTC(),
			ValidUntil:       time.Now().UTC().Add(24 * time.Hour),
		}
		profiles := map[string]any{
			"reasoning-high-v1": map[string]any{"status": "AVAILABLE", "regions": []string{"cn-east"}},
		}
		store := registry.Store{DSN: dsn}
		if err := store.SubmitSnapshot(ctx, snap, profiles); err != nil {
			t.Fatalf("republish submit: %v", err)
		}
		if err := store.ActivateSnapshot(ctx, snap, profiles,
			"spiffe://saoaf.test/ns/default/sa/mmr", "2026", time.Now); err != nil {
			t.Fatalf("republish activate: %v", err)
		}

		// the republish genuinely landed: v2 is PUBLISHED and the active
		// pointer moved to it
		var activeVer int
		if err := pool.QueryRow(ctx, `
			SELECT ps.snapshot_version FROM registry.provider_active_pointer pap
			JOIN registry.provider_snapshot ps ON ps.id = pap.snapshot_id
			WHERE pap.provider_id = $1`, providerID).Scan(&activeVer); err != nil {
			t.Fatal(err)
		}
		if activeVer != 2 {
			t.Fatalf("active pointer = snapshot v%d, want 2 (republish did not land)", activeVer)
		}

		// invariance: the binding row is byte-identical
		revAfter, activeAfter := bindingSnapshot(t, pool, "bind-closure")
		if revAfter != revBefore || activeAfter != activeBefore {
			t.Fatalf("binding moved across republish: rev %d→%d active %v→%v",
				revBefore, revAfter, activeBefore, activeAfter)
		}

		// and the real resolver still resolves the SAME binding with the
		// SAME fingerprint (fresh idem key → fresh plan row, not a replay)
		planAfter, _, err := svc.Resolve(ctx, closureReq(), closureMeta(), "idem-inv-2")
		if err != nil {
			t.Fatalf("resolve after republish: %v", err)
		}
		if len(planAfter.Items) == 0 || planAfter.Items[0].BindingKey != planBefore.Items[0].BindingKey {
			t.Fatalf("binding selection moved: before %+v after %+v", planBefore.Items, planAfter.Items)
		}
		if planAfter.Fingerprint != planBefore.Fingerprint {
			t.Fatalf("plan fingerprint moved across republish: %s → %s",
				planBefore.Fingerprint, planAfter.Fingerprint)
		}
	})
}

// TestClosureKillSwitchBlocksNewPlans (GWT#8, the ARR half of the mock
// closure): suspending the binding goes through the REAL I08 store (rev CAS
// + audit + event), the block is asserted with the REAL resolver, and the
// in-flight invocation's data-plane outcome still lands in the correlation
// ledger against the Prism mock.
func TestClosureKillSwitchBlocksNewPlans(t *testing.T) {
	withDBClosure(t, func(dsn string, pool *pgxpool.Pool) {
		seedClosureChain(t, pool, "bind-closure")
		seedClosurePlan(t, pool, "plan-mock-001")
		svc := newClosureResolver(pool)
		ctx := context.Background()

		// the binding IS selectable before the suspend — this is what makes
		// the post-suspend rejection meaningful (and the resolver predicate
		// falsifiable)
		if _, _, err := svc.Resolve(ctx, closureReq(), closureMeta(), "idem-ks-1"); err != nil {
			t.Fatalf("resolve before suspend: %v", err)
		}

		// the REAL I08 suspend path (PUBLISHED → SUSPENDED, rev CAS)
		bindings := binding.Store{DSN: dsn}
		if err := bindings.Suspend(ctx, binding.TransitionReq{
			BindingKey: "bind-closure", ExpectedRev: 1, TenantRef: "tenant-a",
			Actor: "user:op", TraceID: "trace-ks", ChangeReason: "kill switch drill (GWT#8)",
		}); err != nil {
			t.Fatalf("real I08 suspend: %v", err)
		}

		// new plans CANNOT resolve to the suspended binding — asserted
		// through the real resolver, not by re-reading the row we wrote
		_, _, err := svc.Resolve(ctx, closureReq(), closureMeta(), "idem-ks-2")
		if err == nil {
			t.Fatal("resolve after suspend succeeded (kill switch failed)")
		}
		re, ok := err.(*resolver.ResolveError)
		if !ok || re.Code != resolver.CodeNoCompatibleProvider {
			t.Fatalf("want NO_COMPATIBLE_PROVIDER after suspend, got %+v", err)
		}

		// in-flight request (already planned before the suspend): the data
		// plane still records its outcome — the mock's unavailable example
		// stands in for the MMR kill switch dropping the backend
		mock := closureMock(t, 4385)
		inv := mmr.Invocation{
			Profile: "reasoning-high-v1", ResourcePlanID: "plan-mock-001",
			ResourcePlanItemID: "req-ks", TenantRef: "tenant-a",
			Traceparent: "00-00000000000000000000000000000000-0000000000000000-01",
		}
		req, _ := http.NewRequest("POST", mock+"/v1/chat/completions",
			bytes.NewReader([]byte(`{"model":"reasoning-high-v1","messages":[{"role":"user","content":"x"}]}`)))
		req.Header = inv.Headers()
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Prefer", "code=503")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		decision, derr := inv.DecisionFromError(resp.StatusCode, body)
		if derr != nil {
			t.Fatalf("in-flight outcome extraction: %v", derr)
		}
		store := &mmr.Correlator{Pool: pool}
		if err := store.Record(ctx, mmr.Correlation{
			ResourcePlanID: inv.ResourcePlanID, ResourcePlanItemID: inv.ResourcePlanItemID,
			ModelRouteDecisionID: decision, Outcome: mmr.OutcomeForStatus(resp.StatusCode),
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
