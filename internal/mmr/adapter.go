// Package mmr is the SAOAF-side thin MMR adapter (I11, module 03.4,
// specs/multi-model-router-integration.md). It is a HARNESS-SIDE library:
// model requests, responses, and streaming bytes NEVER pass through ARR
// (权威边界 §2) — the adapter only
//
//   - builds the invocation contract for the Agent Harness (logical
//     profile + correlation headers + W3C trace context, specs §4),
//   - validates the MMR response carries the enterprise evidence extension
//     (model_route_decision_id) and records the 父子决策关联 ledger row
//     for all five outcomes (success/fallback/quota/timeout/failure),
//   - stores the 灰度 routing mode per tenant/agent with audited
//     transitions (shadow → 单 Agent 灰度 → 全量；回退 = 旧静态配置).
//
// Forbidden by the MMR baseline (vLLM Semantic Router integration
// boundary): reading or persisting canonical YAML, Signals, Decisions,
// candidate models, provider endpoints, weights, cascade paths, or
// backend health. The forbid-field regression lives in forbid_test.go.
//
// Module boundary (ADR-0006): SQL over the shared saoaf schema only;
// imports platform packages only.
package mmr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Outcomes (specs §5 失败语义 — 五结局各留双 ID 关联证据).
const (
	OutcomeSucceeded = "SUCCEEDED"
	OutcomeFallback  = "FALLBACK"
	OutcomeQuota     = "QUOTA"
	OutcomeTimeout   = "TIMEOUT"
	OutcomeFailed    = "FAILED"
)

// Correlation errors.
var (
	ErrMissingDecisionID   = errors.New("mmr: response missing model_route_decision_id")
	ErrCorrelationRejected = errors.New("mmr: correlation rejected")
)

// Invocation is the contract the Agent Harness uses to call MMR directly.
// It deliberately carries NO model payload — prompts live in the Harness;
// ARR never sees request/response/streaming bytes.
type Invocation struct {
	Profile            string // logical profile id (from the Plan item)
	ResourcePlanID     string
	ResourcePlanItemID string
	TenantRef          string
	Traceparent        string // W3C trace context propagated end-to-end
	StreamingRequired  bool
}

// Headers renders the runtime contract headers (spec §4 / the Phase 0
// mock contract): correlation IDs + tenant + traceparent. The model body
// is the Harness's business; the mock contract expects the logical
// profile as the `model` field — the harness sets that from Inv.Profile.
func (inv Invocation) Headers() http.Header {
	h := http.Header{}
	h.Set("X-Resource-Plan-ID", inv.ResourcePlanID)
	h.Set("X-Resource-Plan-Item-ID", inv.ResourcePlanItemID)
	h.Set("X-Tenant-Ref", inv.TenantRef)
	if inv.Traceparent != "" {
		h.Set("traceparent", inv.Traceparent)
	}
	return h
}

// ValidateResponse checks the enterprise evidence extension on the MMR
// response: the decision ID must be present; the echoed plan ID must
// match the invocation (透传一致性). Returns the decision for correlation.
func (inv Invocation) ValidateResponse(resp *http.Response) (string, error) {
	decisionID := strings.TrimSpace(resp.Header.Get("X-Model-Route-Decision-ID"))
	if decisionID == "" {
		return "", ErrMissingDecisionID
	}
	if echoed := resp.Header.Get("X-Resource-Plan-ID"); echoed != "" && echoed != inv.ResourcePlanID {
		return "", fmt.Errorf("%w: echoed plan %q != invocation %q",
			ErrCorrelationRejected, echoed, inv.ResourcePlanID)
	}
	return decisionID, nil
}

// ErrorEnvelope is the MMR error body carrying the enterprise evidence
// extension even on failures (specs §4/§5: 五结局都要可关联).
type ErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	ModelRouteDecisionID string `json:"model_route_decision_id"`
	ResourcePlanID       string `json:"resource_plan_id"`
}

// DecisionFromError extracts model_route_decision_id from a non-200 MMR
// response body (the mock contract returns the extension in the error
// body; error semantics stay MMR-owned — never translated into ARR
// "model selection" errors, specs §5).
func (inv Invocation) DecisionFromError(status int, body []byte) (string, error) {
	var env ErrorEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return "", fmt.Errorf("mmr: unreadable error envelope: %w", err)
	}
	if env.ModelRouteDecisionID == "" {
		// no decision id on failures like PROFILE_NOT_FOUND — correlation
		// is impossible; the harness records the error without a decision id
		return "", ErrMissingDecisionID
	}
	if env.ResourcePlanID != "" && env.ResourcePlanID != inv.ResourcePlanID {
		return "", fmt.Errorf("%w: echoed plan mismatch", ErrCorrelationRejected)
	}
	return env.ModelRouteDecisionID, nil
}

// OutcomeForStatus maps an MMR HTTP status to the correlation outcome for
// bookkeeping (specs §5 失败语义).
func OutcomeForStatus(status int) string {
	switch {
	case status == 200:
		return OutcomeSucceeded
	case status == 429:
		return OutcomeQuota
	case status == 503:
		return OutcomeFailed
	default:
		return OutcomeFailed
	}
}

// Correlation is one 父子决策关联 evidence row (I12 consumes this ledger).
type Correlation struct {
	ResourcePlanID       string
	ResourcePlanItemID   string
	ModelRouteDecisionID string
	Outcome              string
	UsageRef             string
	TraceID              string
}

// Correlator persists correlation rows over the shared saoaf schema.
type Correlator struct{ Pool *pgxpool.Pool }

// Record writes one five-outcome correlation row. Duplicate
// (item, decision) pairs are absorbed idempotently (at-least-once record
// path from harness retries — GWT#5 崩溃恢复重放).
func (c Correlator) Record(ctx context.Context, corr Correlation) error {
	if corr.Outcome == "" {
		return fmt.Errorf("mmr: outcome required (one of SUCCEEDED/FALLBACK/QUOTA/TIMEOUT/FAILED)")
	}
	if corr.ResourcePlanID == "" || corr.ResourcePlanItemID == "" || corr.ModelRouteDecisionID == "" {
		return fmt.Errorf("mmr: correlation requires both resource_plan(+item) and decision ids")
	}
	_, err := c.Pool.Exec(ctx, `
		INSERT INTO saoaf.model_route_correlation
			(resource_plan_id, resource_plan_item_id, model_route_decision_id,
			 outcome, usage_ref, trace_id)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (resource_plan_item_id, model_route_decision_id) DO NOTHING`,
		corr.ResourcePlanID, corr.ResourcePlanItemID, corr.ModelRouteDecisionID,
		corr.Outcome, corr.UsageRef, corr.TraceID)
	return err
}

// RoutingMode is the 灰度 state machine state for one scope.
type RoutingMode struct {
	ScopeTenant      string
	ScopeAgent       string
	Mode             string // SHADOW / GRAY / FULL / ROLLED_BACK
	StaticProfileRef string // 旧静态配置回退引用（回退 = 切此配置）
	UpdatedBy        string
	UpdatedAt        time.Time
}

// Mode transitions (灰度状态机): shadow → 单 Agent 灰度 → 全量；回退到
// ROLLED_BACK 可从任意态发生（保留已产生的 Plan/Evidence）。
var legalModeTransitions = map[string]map[string]bool{
	"SHADOW":      {"GRAY": true, "ROLLED_BACK": true},
	"GRAY":        {"FULL": true, "SHADOW": true, "ROLLED_BACK": true},
	"FULL":        {"ROLLED_BACK": true},
	"ROLLED_BACK": {"SHADOW": true, "GRAY": true},
}

// ValidateModeTransition reports whether from→to is legal. An ABSENT
// current state (from == "") is the INITIAL write for a scope — the
// starting mode is the operator's decision (audited via change_record);
// every subsequent transition must follow the machine. The recommended
// roll-out path is SHADOW → GRAY → FULL (specs 灰度状态机).
func ValidateModeTransition(from, to string) bool {
	if from == "" {
		return to == "SHADOW" || to == "GRAY" || to == "FULL" || to == "ROLLED_BACK"
	}
	return legalModeTransitions[from][to]
}

// ModeStore persists routing modes (audited via change_record — 历史不删).
type ModeStore struct{ Pool *pgxpool.Pool }

// Mode returns the routing mode for the most specific matching scope:
// agent-level beats tenant-level beats global (” , ”).
func (s ModeStore) Mode(ctx context.Context, tenant, agent string) (*RoutingMode, error) {
	// most specific first
	for _, scope := range [][2]string{{tenant, agent}, {tenant, ""}, {"", ""}} {
		var m RoutingMode
		err := s.Pool.QueryRow(ctx, `
			SELECT scope_tenant, scope_agent, mode, static_profile_ref, updated_by, updated_at
			FROM saoaf.mmr_routing_mode
			WHERE scope_tenant = $1 AND scope_agent = $2`, scope[0], scope[1]).
			Scan(&m.ScopeTenant, &m.ScopeAgent, &m.Mode, &m.StaticProfileRef, &m.UpdatedBy, &m.UpdatedAt)
		if err == nil {
			return &m, nil
		}
		if err.Error() == "no rows in result set" {
			continue
		}
		return nil, err
	}
	return nil, nil // no configured mode: the harness uses its current default
}

// SetMode transitions the scope's mode with CAS on the previous value and
// a change_record audit row (状态变更审计；历史经 change_record 保留).
func (s ModeStore) SetMode(ctx context.Context, m RoutingMode, expectedFrom string) error {
	if !ValidateModeTransition(expectedFrom, m.Mode) {
		return fmt.Errorf("mmr: illegal routing-mode transition %q → %q (灰度状态机)", expectedFrom, m.Mode)
	}
	tag, err := s.Pool.Exec(ctx, `
		INSERT INTO saoaf.mmr_routing_mode
			(scope_tenant, scope_agent, mode, static_profile_ref, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, now())
		ON CONFLICT (scope_tenant, scope_agent) DO UPDATE
		SET mode = $3, static_profile_ref = $4, updated_by = $5, updated_at = now()
		WHERE saoaf.mmr_routing_mode.mode = $6`,
		m.ScopeTenant, m.ScopeAgent, m.Mode, m.StaticProfileRef, m.UpdatedBy, expectedFrom)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("mmr: routing mode CAS failed (concurrent transition won)")
	}
	// the audit row requires a non-empty tenant_ref; the GLOBAL scope (''
	// tenant) audits under the sentinel `_global`
	auditTenant := m.ScopeTenant
	if auditTenant == "" {
		auditTenant = "_global"
	}
	_, err = s.Pool.Exec(ctx, `
		INSERT INTO saoaf.change_record
			(tenant_ref, actor, trace_id, entity_kind, entity_id, operation, decision_ref, summary)
		VALUES ($1, $2, '', 'mmr-routing-mode', $3, 'TRANSITION', '',
		        jsonb_build_object('from', $4::text, 'to', $5::text, 'static_profile_ref', $6::text))`,
		auditTenant, m.UpdatedBy, m.ScopeTenant+"/"+m.ScopeAgent, expectedFrom, m.Mode, m.StaticProfileRef)
	return err
}
