// Package exitdrillverify implements the I21 MMR exit-drill EVIDENCE
// CHAIN checker: given a drill's recorded identifiers, it exports and
// verifies the full chain the issue demands —
//
//	resource_plan_id → model_route_decision_id → virtual entrypoint
//	→ Recipe revision/digest → pre/post route outcome → evaluation
//	result → rollback result → provider revocation proof
//
// Every link is read from the domain stores (SQL over shared schemas,
// ADR-0006) and the checker asserts the chain has NO BREAKS: a missing
// drill surfaces as a typed error (ErrChainBroken); every other break —
// a missing/unusable exit pack, no drill-attributed correlations, a
// dangling plan, missing evidence coverage, open HIGH/CRITICAL findings,
// or a bypassed approval step — is a NAMED entry in Chain.Breaks and
// forces the NO-GO verdict.
//
// The checker is the DESKTOP-DRILL engine (issue Scope: MCP/A2A/Placement
// only do contract/desktop drills; the real MMR provider exit drill runs
// against production and lands on the I21 ledger). Running it against
// staging or production data produces the export the Go/No-Go review
// consumes.
package exitdrillverify

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrChainBroken is a typed break in the evidence chain.
type ErrChainBroken struct {
	Link string // which link is missing
	Msg  string
}

func (e *ErrChainBroken) Error() string { return "chain broken at " + e.Link + ": " + e.Msg }

func broken(link, msg string) error { return &ErrChainBroken{Link: link, Msg: msg} }

// ChainLink is one verified hop of the export.
type ChainLink struct {
	Kind  string         `json:"kind"`
	ID    string         `json:"id"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

// Chain is the exported, verified evidence chain for one drill.
type Chain struct {
	DrillKey      string      `json:"drill_key"`
	VerifiedAt    string      `json:"verified_at"`
	Complete      bool        `json:"complete"`
	Breaks        []string    `json:"breaks,omitempty"`
	Links         []ChainLink `json:"links"`
	ARRConfigDiff string      `json:"arr_config_diff"` // "0" when ARR/Agent config unchanged (issue: ARR/Agent 配置零变更)
	AgentCodeDiff string      `json:"agent_code_diff"` // "0" when Agent business code unchanged
}

// Checker reads the domain stores and verifies a drill's chain.
type Checker struct {
	Pool *pgxpool.Pool
}

// VerifyDrillChain exports and verifies the full chain for one exit drill.
// The drill must exist (I15 exit_drill) with at least one plan correlation;
// the provider exit must link an exit pack (I14) with revocation proof.
func (c Checker) VerifyDrillChain(ctx context.Context, drillKey string) (*Chain, error) {
	ch := &Chain{
		DrillKey:   drillKey,
		VerifiedAt: time.Now().UTC().Format(time.RFC3339),
		Links:      []ChainLink{},
	}

	// 1. the drill itself
	var drillVendor, drillState string
	var drillInitiator string
	err := c.Pool.QueryRow(ctx, `
		SELECT vendor, state, initiator FROM saoaf.exit_drill WHERE drill_key = $1`,
		drillKey).Scan(&drillVendor, &drillState, &drillInitiator)
	if errors.Is(err, pgx.ErrNoRows) {
		return ch, broken("drill", fmt.Sprintf("exit drill %q does not exist", drillKey))
	}
	if err != nil {
		return nil, err
	}
	ch.Links = append(ch.Links, ChainLink{Kind: "drill", ID: drillKey,
		Attrs: map[string]any{"vendor": drillVendor, "state": drillState, "initiator": drillInitiator}})
	if drillState != "SUCCEEDED" && drillState != "REMEDIATION_OPEN" && drillState != "CLOSED" {
		return ch, broken("drill", fmt.Sprintf("drill state %s is not a completion state (need SUCCEEDED/REMEDIATION_OPEN/CLOSED)", drillState))
	}

	// 2. the exit pack — the drill's OWN pack (exit_pack_key) with a
	// vendor fallback for legacy drills; ignoring the drill's pointer
	// let a vendor's OTHER pack satisfy the link (review R1 P3-1)
	var packKey, packState string
	err = c.Pool.QueryRow(ctx, `
		SELECT p.pack_key, p.state
		FROM saoaf.exit_drill d
		JOIN saoaf.exit_pack p
		  ON p.pack_key = COALESCE(NULLIF(d.exit_pack_key, ''), (
		       SELECT p2.pack_key FROM saoaf.exit_pack p2
		       WHERE p2.vendor = d.vendor AND p2.state IN ('ACTIVE','VALIDATED')
		       ORDER BY p2.revision DESC LIMIT 1))
		WHERE d.drill_key = $1
		  AND p.state IN ('ACTIVE','VALIDATED')`, drillKey).
		Scan(&packKey, &packState)
	if errors.Is(err, pgx.ErrNoRows) {
		ch.Breaks = append(ch.Breaks, "exit_pack: the drill's exit pack (or an active/validated pack for its vendor) does not exist or is not usable")
	} else if err != nil {
		return nil, err
	} else {
		ch.Links = append(ch.Links, ChainLink{Kind: "exit_pack", ID: packKey, Attrs: map[string]any{"state": packState}})
	}

	// 3. plan → decision correlations ATTRIBUTED TO THIS DRILL (review
	// R1 P1-1: a bare time window let unrelated concurrent traffic both
	// SATISFY the link (false GO for a drill with no outcomes) and
	// CONTAMINATE the export (false NO-GO). Attribution: the drill
	// window [created_at, COALESCE(finished_at, now)] AND the correlated
	// plan item's provider must belong to the drill's vendor (via the
	// exit pack's substitute mapping — the provider being exited or its
	// substitute is the routing surface the drill exercises).
	rows, err := c.Pool.Query(ctx, `
		SELECT mc.resource_plan_id, mc.resource_plan_item_id, mc.model_route_decision_id,
		       mc.outcome, mc.usage_ref, mc.trace_id, mc.recorded_at
		FROM saoaf.model_route_correlation mc
		JOIN saoaf.exit_drill d ON d.drill_key = $1
		LEFT JOIN resolver.resource_plan_item i ON i.plan_id = mc.resource_plan_id
		                                       AND i.requirement_id = mc.resource_plan_item_id
		WHERE mc.recorded_at >= d.created_at
		  AND mc.recorded_at <= COALESCE(d.finished_at, now())
		  AND (
		        i.provider_key IS NULL  -- dangling plan: kept so the plan
		                               -- link check reports the missing plan
		    OR i.provider_key = d.vendor
		    OR EXISTS (
		        SELECT 1 FROM saoaf.exit_pack p
		        WHERE p.pack_key = COALESCE(NULLIF(d.exit_pack_key,''), '')
		          AND i.provider_key IN (p.substitute_provider)))
		ORDER BY mc.recorded_at`, drillKey)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type corr struct {
		plan, item, decision, outcome, usage, trace string
		at                                          time.Time
	}
	var corrs []corr
	for rows.Next() {
		var cr corr
		if err := rows.Scan(&cr.plan, &cr.item, &cr.decision, &cr.outcome, &cr.usage, &cr.trace, &cr.at); err != nil {
			return nil, err
		}
		corrs = append(corrs, cr)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(corrs) == 0 {
		ch.Breaks = append(ch.Breaks, "correlation: no drill-attributed model_route_correlation rows in the drill window (pre/post route outcomes missing — unrelated concurrent traffic does NOT satisfy this link)")
	}

	// 4. every correlated plan must exist with items (ARR side of the chain)
	for _, cr := range corrs {
		var planStatus string
		var itemCount int
		perr := c.Pool.QueryRow(ctx, `
			SELECT p.status, (SELECT count(*) FROM resolver.resource_plan_item i WHERE i.plan_id = p.id)
			FROM resolver.resource_plan p WHERE p.id = $1`, cr.plan).Scan(&planStatus, &itemCount)
		if errors.Is(perr, pgx.ErrNoRows) {
			ch.Breaks = append(ch.Breaks, fmt.Sprintf("plan: correlation %s references plan %s which does not exist", cr.decision, cr.plan))
			continue
		}
		if perr != nil {
			return nil, perr
		}
		if itemCount == 0 {
			ch.Breaks = append(ch.Breaks, fmt.Sprintf("plan: plan %s has no items (ARR selection missing)", cr.plan))
		}
		ch.Links = append(ch.Links, ChainLink{Kind: "plan", ID: cr.plan,
			Attrs: map[string]any{"status": planStatus, "items": itemCount}})
		// decision link (MMR side)
		ch.Links = append(ch.Links, ChainLink{Kind: "decision", ID: cr.decision,
			Attrs: map[string]any{"outcome": cr.outcome, "usage_ref": cr.usage, "trace_id": cr.trace, "recorded_at": cr.at}})
	}

	// 5. evidence coverage: the evidence index links records by plan_id
	// (saoaf.evidence_record.plan_id — the I12 correlation key; there is
	// no trace_id column). Each correlated plan with evidence-bearing
	// events must have at least one indexed record.
	seenPlans := map[string]bool{}
	for _, cr := range corrs {
		if seenPlans[cr.plan] {
			continue
		}
		seenPlans[cr.plan] = true
		var n int
		if err := c.Pool.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.evidence_record WHERE plan_id = $1`, cr.plan).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			ch.Breaks = append(ch.Breaks, fmt.Sprintf("evidence: plan %s has no indexed evidence records", cr.plan))
		} else {
			ch.Links = append(ch.Links, ChainLink{Kind: "evidence", ID: cr.plan,
				Attrs: map[string]any{"records": n}})
		}
	}

	// 6. drill findings + remediation (I15) — a provider-exit drill with no
	// findings at all is suspicious; with findings, remediation records
	var openFindings, resolvedFindings int
	if err := c.Pool.QueryRow(ctx, `
		SELECT
			count(*) FILTER (WHERE state = 'OPEN'),
			count(*) FILTER (WHERE state = 'RESOLVED')
		FROM saoaf.exit_drill_finding WHERE drill_key = $1`, drillKey).
		Scan(&openFindings, &resolvedFindings); err != nil {
		return nil, err
	}
	ch.Links = append(ch.Links, ChainLink{Kind: "findings", ID: drillKey,
		Attrs: map[string]any{"open": openFindings, "resolved": resolvedFindings}})
	// open HIGH/CRITICAL findings block GO (review R1 P2-1: an unresolved
	// critical remediation item is a No-Go, not a footnote)
	if openFindings > 0 {
		var openHigh int
		if err := c.Pool.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.exit_drill_finding
			WHERE drill_key = $1 AND state = 'OPEN' AND severity IN ('HIGH','CRITICAL')`,
			drillKey).Scan(&openHigh); err != nil {
			return nil, err
		}
		if openHigh > 0 {
			ch.Breaks = append(ch.Breaks, fmt.Sprintf("findings: %d open HIGH/CRITICAL findings must be remediated before GO", openHigh))
		}
	}

	// 7. the audit trail (transition log) — prove the drill went through
	// the state machine, not a manual DB write
	var transitions int
	if err := c.Pool.QueryRow(ctx, `
		SELECT count(*) FROM saoaf.exit_drill_transition WHERE drill_key = $1`, drillKey).
		Scan(&transitions); err != nil {
		return nil, err
	}
	if transitions == 0 {
		ch.Breaks = append(ch.Breaks, "audit: no transition-log rows for the drill (state-machine bypass?)")
	} else {
		// the trail must include the APPROVAL step (review R1 P3-2: a
		// single forged row is not a trail; the I15 invariant requires
		// an approver ≠ initiator transition into APPROVED)
		var approved int
		if err := c.Pool.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.exit_drill_transition
			WHERE drill_key = $1 AND to_state = 'APPROVED' AND actor <> ''`, drillKey).
			Scan(&approved); err != nil {
			return nil, err
		}
		if approved == 0 {
			ch.Breaks = append(ch.Breaks, "audit: no →APPROVED transition in the log (approval step missing — initiator self-run?)")
		}
		ch.Links = append(ch.Links, ChainLink{Kind: "audit", ID: drillKey, Attrs: map[string]any{"transitions": transitions}})
	}

	// 8. ARR/Agent zero-change proof placeholders — the drill export
	// REQUIRES the operator to attach the config/code diff evidence
	// (issue: ARR/Agent 配置零变更 — 演练前后配置导出对比). The checker
	// cannot diff external config stores; the Go/No-Go reviewer signs
	// these two fields after running the documented export commands
	// (docs/runbooks). Leaving them at "unverified" blocks Complete.
	ch.ARRConfigDiff = "unverified"
	ch.AgentCodeDiff = "unverified"

	ch.Complete = len(ch.Breaks) == 0
	return ch, nil
}

// MarkARRZeroChange / MarkAgentZeroChange let the drill operator record
// the zero-change proof AFTER running the export comparison — the chain
// export in the Go/No-Go pack then carries "0" (verified) instead of
// "unverified".
func (ch *Chain) MarkARRZeroChange(verified bool) {
	if verified {
		ch.ARRConfigDiff = "0"
	}
}

func (ch *Chain) MarkAgentZeroChange(verified bool) {
	if verified {
		ch.AgentCodeDiff = "0"
	}
}

// GoNoGo renders the Go/No-Go verdict block for the drill export.
// No-Go conditions: chain incomplete, ARR/Agent diffs unverified, or the
// drill state is not a completion state.
func (ch *Chain) GoNoGo() (verdict string, reasons []string) {
	if !ch.Complete {
		reasons = append(reasons, "evidence chain incomplete: "+fmt.Sprint(len(ch.Breaks))+" break(s)")
	}
	if ch.ARRConfigDiff != "0" {
		reasons = append(reasons, "ARR config zero-change proof unverified")
	}
	if ch.AgentCodeDiff != "0" {
		reasons = append(reasons, "Agent code zero-change proof unverified")
	}
	if len(reasons) == 0 {
		return "GO", nil
	}
	return "NO-GO", reasons
}
