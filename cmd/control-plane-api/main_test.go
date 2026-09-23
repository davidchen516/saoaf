package main

// Composition-root integration test (ADR-0006: only cmd may import multiple
// internal modules): proves the policyEngine adapter drives the REAL
// policy store + CEL evaluator behind the resolver's Policy filter step —
// the cross-module integration the resolver package tests fake out.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/policy"
	"github.com/davidchen516/saoaf/internal/resolver"
)

func TestPolicyEngineAdapterIntegration(t *testing.T) {
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
	name := fmt.Sprintf("cmd_policy_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, _ := url.Parse(base)
	u.Path = "/" + name
	db := u.String()

	bin := os.Getenv("GOOSE_BIN")
	if bin == "" {
		if bin, err = exec.LookPath("goose"); err != nil {
			t.Skip("goose CLI not found")
		}
	}
	wd, _ := os.Getwd()
	if out, err := exec.Command(bin, "-dir", filepath.Join(wd, "..", "..", "migrations"),
		"postgres", db, "up").CombinedOutput(); err != nil {
		t.Fatalf("goose up: %v\n%s", err, out)
	}

	conn, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(ctx) })

	// deny-all active policy
	content := policy.Content{EligibilityCEL: "false"}
	if _, err := conn.Exec(ctx, `
		INSERT INTO policy.policy_revision
			(set_id, version, state, content, content_digest, published_at, activated_at)
		VALUES ('resolver-eligibility', 1, 'ACTIVATED', $1::jsonb, $2, now(), now())`,
		`{"regions":[],"data_classification_max":"","vendor_restrictions":[],"export_requirements":[],"eligibility_cel":"false"}`,
		content.CanonicalDigest()); err != nil {
		t.Fatal(err)
	}
	// capability/provider/snapshot/binding
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.capability_definition
			(capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		VALUES ('model.cmd.test', 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.resource_provider
			(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		VALUES ('prov-cmd', 'MODEL', 'svc://cmd/mmr', 'user:t', 'spiffe://saoaf.test/ns/default/sa/mmr', 'PUBLISHED', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.provider_snapshot
			(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
			 profiles, generated_at, valid_until, state)
		VALUES (1, 1, '2026.09', 'sha256:5555555555555555555555555555555555555555555555555555555555555555', 'sig',
		        'spiffe://saoaf.test/ns/default/sa/mmr',
		        '[{"profile_id":"cmd-prof","capability_keys":["model.cmd.test"],"regions":["cn-east"],"data_classification_max":"CONFIDENTIAL","status":"AVAILABLE"}]'::jsonb,
		        now(), now() + interval '1 hour', 'PUBLISHED')`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.provider_active_pointer (provider_id, snapshot_id)
		SELECT 1, id FROM registry.provider_snapshot WHERE snapshot_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active)
		SELECT 'bind-cmd', cd.id, 1, ps.id, 'cmd-prof', 'production',
		       '{"tenant_refs":[],"factory_refs":[],"regions":[],"agent_refs":[]}'::jsonb,
		       'sha256:6666666666666666666666666666666666666666666666666666666666666666', 100, 'PUBLISHED', 1, TRUE
		FROM registry.capability_definition cd, registry.provider_snapshot ps
		WHERE cd.capability_key = 'model.cmd.test' AND ps.snapshot_version = 1`); err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	engine := policyEngine{
		store: &policy.Store{DSN: db, Pool: pool},
		eval:  policy.NewEvaluator(),
	}
	svc := &resolver.Service{
		Plans: &resolver.Store{Pool: pool},
		Cache: resolver.NewSnapshotCache(resolver.NewRegistrySnapshotLoader(pool), 5*time.Second),
		Pool:  pool, Policy: engine, PolicySetID: "resolver-eligibility",
		Now: time.Now, NewID: resolver.NewPlanID,
		DefaultTTL: 300 * time.Second, MaxTTL: 3600 * time.Second,
	}
	req := &resolver.Request{
		ContractVersion: "1.0", TaskRef: "task-cmd",
		Requirements: []resolver.Requirement{{
			RequirementID: "req-1", CapabilityID: "model.cmd.test",
			CapabilityMajorVersion: 1, ResourceType: "MODEL_PROVIDER",
			Constraints: map[string]any{"region": "cn-east"},
		}},
	}
	// deny-all active policy → resolve rejected with POLICY_DENIED detail
	_, _, err = svc.Resolve(ctx, req, resolver.CallerMeta{
		CallerRef: "user:t", TenantRef: "tenant:t", Environment: "production",
	}, "idem-cmd-1")
	if err == nil {
		t.Fatal("deny-all policy accepted the resolve")
	}
	re, ok := err.(*resolver.ResolveError)
	if !ok || re.Code != resolver.CodeNoCompatibleProvider ||
		len(re.Details) == 0 || re.Details[0].ReasonCodes[0] != resolver.ReasonPolicyDenied {
		t.Fatalf("want NO_COMPATIBLE_PROVIDER/POLICY_DENIED, got %+v", err)
	}

	// allow-all policy → resolve succeeds and carries the revision
	if _, err := conn.Exec(ctx, `
		INSERT INTO policy.policy_revision
			(set_id, version, state, content, content_digest, published_at, activated_at)
		VALUES ('resolver-eligibility', 2, 'ACTIVATED', $1::jsonb, $2, now(), now())`,
		`{"regions":[],"data_classification_max":"","vendor_restrictions":[],"export_requirements":[],"eligibility_cel":"true"}`,
		func() string { c := policy.Content{EligibilityCEL: "true"}; return c.CanonicalDigest() }()); err != nil {
		// the partial unique index rejects a second ACTIVE; supersede first
		if _, uerr := conn.Exec(ctx, `
			UPDATE policy.policy_revision SET state = 'SUPERSEDED'
			WHERE set_id = 'resolver-eligibility' AND version = 1`); uerr != nil {
			t.Fatal(uerr)
		}
		if _, ierr := conn.Exec(ctx, `
			INSERT INTO policy.policy_revision
				(set_id, version, state, content, content_digest, published_at, activated_at)
			VALUES ('resolver-eligibility', 2, 'ACTIVATED', $1::jsonb, $2, now(), now())`,
			`{"regions":[],"data_classification_max":"","vendor_restrictions":[],"export_requirements":[],"eligibility_cel":"true"}`,
			func() string { c := policy.Content{EligibilityCEL: "true"}; return c.CanonicalDigest() }()); ierr != nil {
			t.Fatal(ierr)
		}
	}
	plan, _, err := svc.Resolve(ctx, req, resolver.CallerMeta{
		CallerRef: "user:t", TenantRef: "tenant:t", Environment: "production",
	}, "idem-cmd-2")
	if err != nil {
		t.Fatalf("allow-all policy resolve: %v", err)
	}
	if plan.PolicySetID != "resolver-eligibility" || plan.PolicyVersion != 2 {
		t.Fatalf("policy revision not carried: %s@%d", plan.PolicySetID, plan.PolicyVersion)
	}
}
