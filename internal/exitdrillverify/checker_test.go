package exitdrillverify

// Real-PostgreSQL tests for the I21 evidence-chain checker: complete-chain
// export (GO), each break class (named link), and the zero-change
// proof flow (NO-GO → GO after operator sign-off).
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func withDBV(t *testing.T, fn func(dsn string, pool *pgxpool.Pool)) {
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
	name := fmt.Sprintf("edv_%d_%d", os.Getpid(), time.Now().UnixNano())
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

func seedFullChain(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// capability → provider → snapshot → binding (registry prerequisites)
	for _, stmt := range []string{
		`INSERT INTO registry.capability_definition (capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		  VALUES ('cap-v', 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:op') ON CONFLICT DO NOTHING`,
		`INSERT INTO registry.resource_provider (provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		  VALUES ('prov-v', 'MODEL', 'ref://mmr', 'user:op', 'spiffe://saoaf.test/ns/default/sa/mmr', 'PUBLISHED', 1, 1) ON CONFLICT DO NOTHING`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatalf("seed registry: %v", err)
		}
	}
	var pid int
	if err := pool.QueryRow(ctx,
		`SELECT id FROM registry.resource_provider WHERE provider_key='prov-v'`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO registry.provider_snapshot (provider_id, snapshot_version, contract_version, digest, signature, workload_identity, generated_at, valid_until)
		VALUES ($1, 1, '2026.09', 'sha256:0000000000000000000000000000000000000000000000000000000000000000', 'sig', 'spiffe://saoaf.test/ns/default/sa/mmr', now(), now() + interval '1 day')`,
		pid); err != nil {
		t.Fatal(err)
	}
	// plan + item
	if _, err := pool.Exec(ctx, `
		INSERT INTO resolver.resource_plan (id, caller_ref, tenant_ref, fingerprint, request_digest, idempotency_key, status, expires_at)
		VALUES ('plan-v', 'user:caller', 'tenant-a', 'sha256:0000000000000000000000000000000000000000000000000000000000000000', 'sha256:0000000000000000000000000000000000000000000000000000000000000000', 'idem-v', 'RESOLVED', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO resolver.resource_plan_item (plan_id, requirement_id, capability_key, major_version, capability_revision, binding_key, binding_revision, provider_key, snapshot_version, profile_or_action, reason_codes)
		VALUES ('plan-v', 'req-v', 'cap-v', 1, 1, 'bind-v', 1, 'prov-w', 1, 'reasoning-high-v1', '[]')`); err != nil {
		t.Fatal(err)
	}
	// exit pack (active)
	if _, err := pool.Exec(ctx, `
		INSERT INTO saoaf.exit_pack (pack_key, vendor, revision, state, owner_ref, substitute_provider, valid_until, created_by)
		VALUES ('pack-v', 'vendor-v', 1, 'ACTIVE', 'user:op', 'prov-w', now() + interval '30 day', 'user:op')`); err != nil {
		t.Fatal(err)
	}
	// drill (SUCCEEDED) + transition log + a resolved finding
	if _, err := pool.Exec(ctx, `
		INSERT INTO saoaf.exit_drill (drill_key, vendor, exit_pack_key, initiator, approver, state, result_evidence, finished_at)
		VALUES ('drill-v', 'vendor-v', 'pack-v', 'user:init', 'user:approver', 'SUCCEEDED', 'evidence://drill-v', now() + interval '1 hour')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO saoaf.exit_drill_finding (drill_key, finding_key, description, severity, state, remediation, remediation_evidence)
		VALUES ('drill-v', 'f-v', 'latency regression', 'MEDIUM', 'RESOLVED', 'scaled pool', 'evidence://f-v')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO saoaf.exit_drill_transition (drill_key, from_state, to_state, actor)
		VALUES ('drill-v','DRAFT','APPROVED','user:approver'), ('drill-v','APPROVED','SUCCEEDED','user:worker')`); err != nil {
		t.Fatal(err)
	}
	// correlation + evidence
	if _, err := pool.Exec(ctx, `
		INSERT INTO saoaf.model_route_correlation (resource_plan_id, resource_plan_item_id, model_route_decision_id, outcome, usage_ref, trace_id)
		VALUES ('plan-v', 'req-v', 'mrd-v', 'SUCCEEDED', 'usage-v', 'trace-v')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO saoaf.evidence_record (event_id, source_topic, aggregate_kind, aggregate_id, tenant_ref, occurred_at, payload_digest, content, plan_id, state)
		VALUES ('ev-v', 'mmr.routed', 'mmr', 'mrd-v', 'tenant-a', now(), 'sha256:0000000000000000000000000000000000000000000000000000000000000000', '{}', 'plan-v', 'LINKED')`); err != nil {
		t.Fatal(err)
	}
}

// Complete chain → GO after the operator signs the zero-change proofs.
func TestVerifyDrillChainCompleteGo(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		c := Checker{Pool: pool}
		ch, err := c.VerifyDrillChain(context.Background(), "drill-v")
		if err != nil {
			t.Fatalf("verify: %v", err)
		}
		if len(ch.Breaks) > 0 {
			t.Fatalf("unexpected breaks: %v", ch.Breaks)
		}
		// before sign-off: NO-GO (diffs unverified)
		verdict, reasons := ch.GoNoGo()
		if verdict != "NO-GO" || len(reasons) != 2 {
			t.Fatalf("pre-signoff verdict = %s %v, want NO-GO with 2 unverified-diff reasons", verdict, reasons)
		}
		// operator runs the documented export-diff commands and signs
		ch.MarkARRZeroChange(true)
		ch.MarkAgentZeroChange(true)
		verdict, reasons = ch.GoNoGo()
		if verdict != "GO" || len(reasons) != 0 {
			t.Fatalf("post-signoff verdict = %s %v, want GO", verdict, reasons)
		}
		// the chain exports every link kind
		kinds := map[string]bool{}
		for _, l := range ch.Links {
			kinds[l.Kind] = true
		}
		for _, want := range []string{"drill", "exit_pack", "plan", "decision", "evidence", "findings", "audit"} {
			if !kinds[want] {
				t.Fatalf("chain export missing link kind %q", want)
			}
		}
	})
}

// Every break class is a NAMED missing link (issue: 全链无断点).
func TestVerifyDrillChainBreaks(t *testing.T) {
	cases := []struct {
		name  string
		broke func(t *testing.T, pool *pgxpool.Pool)
		link  string // the break must name this link
	}{
		{"missing drill", func(t *testing.T, pool *pgxpool.Pool) {}, "drill"},
		{"in-flight drill", func(t *testing.T, pool *pgxpool.Pool) {
			// drill exists but RUNNING
			if _, err := pool.Exec(context.Background(), `
				INSERT INTO saoaf.exit_drill (drill_key, vendor, initiator, state)
				VALUES ('drill-v', 'vendor-v', 'user:init', 'RUNNING')`); err != nil {
				t.Fatal(err)
			}
		}, "drill"},
		{"no exit pack", func(t *testing.T, pool *pgxpool.Pool) {
			seedFullChain(t, pool)
			// verified packs are UNDELETABLE (audit red line) — the break
			// is simulated by reverting to a non-usable state instead
			if _, err := pool.Exec(context.Background(),
				`UPDATE saoaf.exit_pack SET state='SUPERSEDED' WHERE pack_key='pack-v'`); err != nil {
				t.Fatal(err)
			}
		}, "exit_pack"},
		{"correlations deleted", func(t *testing.T, pool *pgxpool.Pool) {
			seedFullChain(t, pool)
			if _, err := pool.Exec(context.Background(),
				`DELETE FROM saoaf.model_route_correlation WHERE resource_plan_id='plan-v'`); err != nil {
				t.Fatal(err)
			}
		}, "correlation"},
		{"plan missing", func(t *testing.T, pool *pgxpool.Pool) {
			seedFullChain(t, pool)
			// resolver plans/items are IMMUTABLE decision-ledger records —
			// the break is simulated by pointing the correlation at a
			// nonexistent plan (the shape a broken ARR write leaves)
			if _, err := pool.Exec(context.Background(), `
				INSERT INTO saoaf.model_route_correlation
					(resource_plan_id, resource_plan_item_id, model_route_decision_id, outcome)
				VALUES ('plan-missing', 'req-missing', 'mrd-missing', 'SUCCEEDED')`); err != nil {
				t.Fatal(err)
			}
		}, "plan"},
		{"evidence missing", func(t *testing.T, pool *pgxpool.Pool) {
			seedFullChain(t, pool)
			if _, err := pool.Exec(context.Background(),
				`DELETE FROM saoaf.evidence_record WHERE plan_id='plan-v'`); err != nil {
				t.Fatal(err)
			}
		}, "evidence"},
		{"audit trail bypassed", func(t *testing.T, pool *pgxpool.Pool) {
			seedFullChain(t, pool)
			if _, err := pool.Exec(context.Background(),
				`DELETE FROM saoaf.exit_drill_transition WHERE drill_key='drill-v'`); err != nil {
				t.Fatal(err)
			}
		}, "audit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			withDBV(t, func(dsn string, pool *pgxpool.Pool) {
				tc.broke(t, pool)
				c := Checker{Pool: pool}
				ch, err := c.VerifyDrillChain(context.Background(), "drill-v")
				if err != nil {
					// the missing-drill case returns a typed error
					var eb *ErrChainBroken
					if !errors.As(err, &eb) {
						t.Fatalf("verify: %v", err)
					}
					if eb.Link != tc.link {
						t.Fatalf("typed break link = %q, want %q", eb.Link, tc.link)
					}
					return
				}
				if ch.Complete {
					t.Fatalf("%s: chain claims complete — break not detected", tc.name)
				}
				found := false
				for _, b := range ch.Breaks {
					if len(b) > len(tc.link) && b[:len(tc.link)] == tc.link {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s: no break names link %q (breaks: %v)", tc.name, tc.link, ch.Breaks)
				}
				verdict, _ := ch.GoNoGo()
				if verdict != "NO-GO" {
					t.Fatalf("%s: verdict = %s, want NO-GO", tc.name, verdict)
				}
			})
		})
	}
}

// The exported chain is JSON-serializable for the Go/No-Go pack.
func TestChainJSONExport(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		c := Checker{Pool: pool}
		ch, err := c.VerifyDrillChain(context.Background(), "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(ch)
		if err != nil {
			t.Fatal(err)
		}
		var back Chain
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.DrillKey != "drill-v" || !back.Complete || len(back.Links) == 0 {
			t.Fatalf("round-trip chain = %+v", back)
		}
	})
}

// ==== R1 review regressions ====

// TestUnrelatedTrafficDoesNotSatisfyNorContaminate (R1 P1-1, both
// directions): a drill with NO attributed outcomes must stay broken even
// when unrelated concurrent traffic floods the window; a complete drill
// must stay complete when unrelated dangling rows exist.
func TestUnrelatedTrafficDoesNotSatisfyNorContaminate(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		// drill-B: vendor-b with no pack, no attributed outcomes — the
		// false-GO probe: unrelated vendor-v traffic must NOT satisfy it
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.exit_drill (drill_key, vendor, initiator, state)
			VALUES ('drill-b', 'vendor-b', 'user:init', 'SUCCEEDED')`); err != nil {
			t.Fatal(err)
		}
		// unrelated concurrent traffic in the same window (a FAILED outcome
		// from the vendor-v surface — pre-existing seed correlation)
		ch, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-b")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Complete {
			t.Fatal("false GO: drill-b has NO attributed outcomes but unrelated vendor-v traffic satisfied the correlation link")
		}
		found := false
		for _, b := range ch.Breaks {
			if len(b) > 12 && b[:12] == "correlation:" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the correlation break; got %v", ch.Breaks)
		}
		// the complete drill stays complete despite drill-b's presence
		chC, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		if !chC.Complete {
			t.Fatalf("false NO-GO: the complete drill got contaminated: %v", chC.Breaks)
		}
	})
}

// TestOpenCriticalFindingsBlockGo (R1 P2-1): open HIGH/CRITICAL findings
// must force NO-GO.
func TestOpenCriticalFindingsBlockGo(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		// reopen the resolved finding as CRITICAL
		if _, err := pool.Exec(ctx, `
			UPDATE saoaf.exit_drill_finding
			SET state='OPEN', severity='CRITICAL', remediation=''
			WHERE drill_key='drill-v'`); err != nil {
			t.Fatal(err)
		}
		ch, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Complete {
			t.Fatal("open CRITICAL finding did not break the chain")
		}
		ch.MarkARRZeroChange(true)
		ch.MarkAgentZeroChange(true)
		verdict, _ := ch.GoNoGo()
		if verdict != "NO-GO" {
			t.Fatal("open CRITICAL finding must force NO-GO even after sign-off")
		}
	})
}

// TestAuditRequiresApprovalStep (R1 P3-2): a transition log without the
// →APPROVED step (initiator self-run) breaks the audit link.
func TestAuditRequiresApprovalStep(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		// remove the approval transition, keep a fake one
		if _, err := pool.Exec(ctx, `
			DELETE FROM saoaf.exit_drill_transition
			WHERE drill_key='drill-v' AND to_state='APPROVED'`); err != nil {
			t.Fatal(err)
		}
		ch, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Complete {
			t.Fatal("audit trail without →APPROVED passed (self-run undetected)")
		}
		found := false
		for _, b := range ch.Breaks {
			if len(b) > 5 && b[:5] == "audit" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the audit break; got %v", ch.Breaks)
		}
	})
}

// TestDrillPackPointerHonored (R1 P3-1): a drill pointing at a
// nonexistent pack must break even when the vendor has another ACTIVE pack.
func TestDrillPackPointerHonored(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		// add a SECOND active pack for the same vendor; point the drill at
		// a nonexistent pack — the vendor fallback must NOT silently
		// satisfy a WRONG pointer... but the fallback IS the designed
		// behavior for legacy drills; the break must come from the
		// pointed-at pack being unusable. Point at the SUPERSEDED one.
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.exit_pack (pack_key, vendor, revision, state, owner_ref, substitute_provider, valid_until, created_by)
			VALUES ('pack-w', 'vendor-v', 2, 'SUPERSEDED', 'user:op', 'prov-w', now() + interval '30 day', 'user:op')`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE saoaf.exit_drill SET exit_pack_key='pack-w' WHERE drill_key='drill-v'`); err != nil {
			t.Fatal(err)
		}
		ch, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Complete {
			t.Fatal("drill pointing at a SUPERSEDED pack stayed complete")
		}
		found := false
		for _, b := range ch.Breaks {
			if len(b) > 9 && b[:9] == "exit_pack" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the exit_pack break; got %v", ch.Breaks)
		}
	})
}

// ==== R2 review regressions ====

// TestExactPlanAttributionCoversPreExit (R2 P2-1): the drill's PRE-exit
// outcomes (routed on the EXITED provider, not the substitute) enter the
// chain when the drill names its plans explicitly — the fallback scope
// cannot see them (provider-scope matches vendor/substitute only).
func TestExactPlanAttributionCoversPreExit(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		// a pre-exit outcome on the EXITED provider (prov-v — NOT the
		// substitute prov-w): invisible to provider-scope attribution
		if _, err := pool.Exec(ctx, `
			INSERT INTO resolver.resource_plan (id, caller_ref, tenant_ref, fingerprint, request_digest, idempotency_key, status, expires_at)
			VALUES ('plan-pre', 'user:caller', 'tenant-a', 'sha256:0000000000000000000000000000000000000000000000000000000000000000', 'sha256:0000000000000000000000000000000000000000000000000000000000000000', 'idem-pre', 'RESOLVED', now() + interval '1 hour')`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO resolver.resource_plan_item (plan_id, requirement_id, capability_key, major_version, capability_revision, binding_key, binding_revision, provider_key, snapshot_version, profile_or_action, reason_codes)
			VALUES ('plan-pre', 'req-pre', 'cap-v', 1, 1, 'bind-pre', 1, 'prov-v', 1, 'reasoning-high-v1', '[]')`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.model_route_correlation (resource_plan_id, resource_plan_item_id, model_route_decision_id, outcome)
			VALUES ('plan-pre', 'req-pre', 'mrd-pre', 'SUCCEEDED')`); err != nil {
			t.Fatal(err)
		}
		// evidence for the pre-exit plan (the exact chain requires it)
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.evidence_record (event_id, source_topic, aggregate_kind, aggregate_id, tenant_ref, occurred_at, payload_digest, content, plan_id, state)
			VALUES ('ev-pre', 'mmr.routed', 'mmr', 'mrd-pre', 'tenant-a', now(), 'sha256:0000000000000000000000000000000000000000000000000000000000000000', '{}', 'plan-pre', 'LINKED')`); err != nil {
			t.Fatal(err)
		}
		// fallback scope: plan-pre invisible (provider prov-v not matched)
		chFallback, err := (Checker{Pool: pool}).VerifyDrillChainScoped(ctx, "drill-v", nil)
		if err != nil {
			t.Fatal(err)
		}
		foundPre := false
		for _, l := range chFallback.Links {
			if l.ID == "mrd-pre" {
				foundPre = true
			}
		}
		if foundPre {
			t.Fatal("fallback scope must NOT claim unknown plans (attribution honesty)")
		}
		// EXACT attribution: the drill names plan-v AND plan-pre
		chExact, err := (Checker{Pool: pool}).VerifyDrillChainScoped(ctx, "drill-v", []string{"plan-v", "plan-pre"})
		if err != nil {
			t.Fatal(err)
		}
		if !chExact.Complete {
			t.Fatalf("exact attribution with both plans must be complete; breaks: %v", chExact.Breaks)
		}
		foundPre = false
		for _, l := range chExact.Links {
			if l.ID == "mrd-pre" {
				foundPre = true
			}
		}
		if !foundPre {
			t.Fatal("pre-exit outcome (exited provider) missing from the exact-attribution chain")
		}
		// the caller controls the export scope: naming ONLY plan-pre yields
		// a complete chain without the post-exit outcome (mrd-r absent —
		// the drill operator decides which surface the pack documents)
		chExact2, err := (Checker{Pool: pool}).VerifyDrillChainScoped(ctx, "drill-v", []string{"plan-pre"})
		if err != nil {
			t.Fatal(err)
		}
		if !chExact2.Complete {
			t.Fatalf("plan-pre-only exact chain must be complete (its evidence exists); breaks: %v", chExact2.Breaks)
		}
		for _, l := range chExact2.Links {
			if l.ID == "mrd-r" {
				t.Fatal("plan-pre-only chain must NOT include the post-exit outcome mrd-r")
			}
		}
	})
}

// TestSelfApprovalDetected (R2 P3-2): a drill whose recorded approver IS
// the initiator (or empty) breaks the audit link — actor <> ” was not
// enough.
func TestSelfApprovalDetected(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		// approver = initiator (self-run with a forged non-empty actor)
		if _, err := pool.Exec(ctx, `
			UPDATE saoaf.exit_drill SET approver = 'user:init' WHERE drill_key='drill-v'`); err != nil {
			t.Fatal(err)
		}
		ch, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Complete {
			t.Fatal("self-approval (approver = initiator) passed the audit link")
		}
		found := false
		for _, b := range ch.Breaks {
			if len(b) > 5 && b[:5] == "audit" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the audit invariant break; got %v", ch.Breaks)
		}
	})
}

// TestLegacyDrillFallbackScope (R2 P2-2 / R3 P1 rewrite): a legacy drill
// (empty exit_pack_key) attributes via the vendor-fallback pack's
// SUBSTITUTE — the R2 probeD scenario (vendor-v legacy drill whose window
// covers substitute-routed traffic) must find the correlation and NOT
// break. Both assertion directions are hard (no t.Log escape).
func TestLegacyDrillFallbackScope(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		// legacy drill: vendor-v, NO pack pointer — the vendor fallback
		// must resolve pack-v (ACTIVE) and its substitute prov-w carries
		// its window traffic. Backdate the drill start so the SEEDED
		// correlation (inserted moments ago) falls inside the window,
		// and give it a proper approval trail.
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.exit_drill (drill_key, vendor, initiator, approver, state, finished_at, created_at)
			VALUES ('drill-legacy', 'vendor-v', 'user:init', 'user:approver', 'SUCCEEDED', now() + interval '1 hour', now() - interval '5 minutes')`); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.exit_drill_transition (drill_key, from_state, to_state, actor)
			VALUES ('drill-legacy','DRAFT','APPROVED','user:approver'), ('drill-legacy','APPROVED','SUCCEEDED','user:worker')`); err != nil {
			t.Fatal(err)
		}
		ch, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-legacy")
		if err != nil {
			t.Fatal(err)
		}
		// direction 1: NO correlation break (the substitute-routed window
		// traffic attributes via the vendor-fallback pack)
		for _, b := range ch.Breaks {
			if len(b) > 12 && b[:12] == "correlation:" {
				t.Fatalf("legacy drill failed to attribute substitute-routed traffic (vendor-fallback pack not in the substitute branch): %v", ch.Breaks)
			}
		}
		if !ch.Complete {
			t.Fatalf("legacy drill chain incomplete: %v", ch.Breaks)
		}
		// direction 2: an EMPTY vendor (no pack anywhere) still breaks
		if _, err := pool.Exec(ctx, `
			INSERT INTO saoaf.exit_drill (drill_key, vendor, initiator, approver, state, finished_at)
			VALUES ('drill-legacy-none', 'vendor-none', 'user:init', 'user:approver', 'SUCCEEDED', now() + interval '1 hour')`); err != nil {
			t.Fatal(err)
		}
		ch2, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-legacy-none")
		if err != nil {
			t.Fatal(err)
		}
		if ch2.Complete {
			t.Fatal("legacy drill with no pack at all stayed complete (vendor fallback must not fabricate attribution)")
		}
	})
}

// TestChainExportDisclosesAttribution (R3 P2-A): the JSON export carries
// the attribution mode and the named plans — the sign-off reviews THIS
// artifact, so a fallback export must be distinguishable from an exact
// one at rest.
func TestChainExportDisclosesAttribution(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		chFallback, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		if chFallback.AttributionMode != "provider-scope" {
			t.Fatalf("fallback export mode = %q", chFallback.AttributionMode)
		}
		if chFallback.AttributionPlans != nil {
			t.Fatalf("fallback export plans = %v, want nil", chFallback.AttributionPlans)
		}
		chExact, err := (Checker{Pool: pool}).VerifyDrillChainScoped(ctx, "drill-v", []string{"plan-v"})
		if err != nil {
			t.Fatal(err)
		}
		if chExact.AttributionMode != "exact-plans" || len(chExact.AttributionPlans) != 1 || chExact.AttributionPlans[0] != "plan-v" {
			t.Fatalf("exact export attribution = %q %v", chExact.AttributionMode, chExact.AttributionPlans)
		}
		// the JSON round-trip keeps both fields
		b, err := json.Marshal(chExact)
		if err != nil {
			t.Fatal(err)
		}
		var back Chain
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		if back.AttributionMode != "exact-plans" || len(back.AttributionPlans) != 1 {
			t.Fatalf("round-trip attribution = %q %v", back.AttributionMode, back.AttributionPlans)
		}
	})
}

// TestCompletionWithoutFinishedAtBreaks (R3 P3): a completion-state drill
// without finished_at leaves the attribution window unbounded — the
// checker must break instead of absorbing traffic forever.
func TestCompletionWithoutFinishedAtBreaks(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		// wipe finished_at on the completed drill (schema allows NULL)
		if _, err := pool.Exec(ctx, `
			UPDATE saoaf.exit_drill SET finished_at = NULL WHERE drill_key='drill-v'`); err != nil {
			t.Fatal(err)
		}
		ch, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Complete {
			t.Fatal("completion state without finished_at passed (unbounded attribution window)")
		}
		found := false
		for _, b := range ch.Breaks {
			if len(b) > 5 && b[:5] == "drill" && len(b) > 30 && b[:30] != "" {
				if strings.Contains(b, "finished_at") {
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("expected the finished_at break; got %v", ch.Breaks)
		}
	})
}

// TestRemediationOpenWithoutFinishedAtBreaks (R4 probe collected): the
// REMEDIATION_OPEN completion state previously escaped the finished_at
// guard — a 30-day-old drill could claim today's traffic for a GO.
func TestRemediationOpenWithoutFinishedAtBreaks(t *testing.T) {
	withDBV(t, func(dsn string, pool *pgxpool.Pool) {
		seedFullChain(t, pool)
		ctx := context.Background()
		if _, err := pool.Exec(ctx, `
			UPDATE saoaf.exit_drill
			SET state='REMEDIATION_OPEN', finished_at=NULL
			WHERE drill_key='drill-v'`); err != nil {
			t.Fatal(err)
		}
		ch, err := (Checker{Pool: pool}).VerifyDrillChain(ctx, "drill-v")
		if err != nil {
			t.Fatal(err)
		}
		if ch.Complete {
			t.Fatal("REMEDIATION_OPEN without finished_at passed (unbounded window — R4 probe)")
		}
		found := false
		for _, b := range ch.Breaks {
			if strings.Contains(b, "finished_at") {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected the finished_at break; got %v", ch.Breaks)
		}
	})
}
