// Runtime Resource Resolver API (I09, specs/resource-resolver-api.md §3):
// resolve + get-plan under the verified-identity chain, with Runtime/Admin
// isolation via disjoint OAuth scopes (runtime callers hold
// resource.resolve/resource.read; admin endpoints require resource.*.write
// scopes — GWT#6).
package httpapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/middleware"
	"github.com/davidchen516/saoaf/internal/resolver"
)

// MountResolver wires /api/ai-resource-resolver/v1 under the fixed chain
// identity(401) → rate limit → scope(403). Health probes are
// unauthenticated (process-level, no dependency data leaks — specs §3.3).
func MountResolver(r chi.Router, cfg *ResolverMountConfig) {
	// chi: Mount appends to the parent's middleware list, so the
	// subrouter MUST be mounted before any direct route lands on the
	// parent — health routes come after the Route() call.
	r.Route("/api/ai-resource-resolver/v1", func(v1 chi.Router) {
		v1.Use(middleware.RequireIdentity(cfg.Authn))
		v1.Use(middleware.RateLimit(cfg.RatePerSec, cfg.RateBurst))
		v1.With(middleware.RequireScope("resource.resolve")).
			Post("/resource-plans:resolve", cfg.handleResolve)
		v1.With(middleware.RequireScope("resource.read")).
			Get("/resource-plans/{id}", cfg.handleGetPlan)
	})

	// health: unauthenticated, under the API base path (specs §3.3;
	// process-level only — no dependency data, no provider catalog)
	r.Get("/api/ai-resource-resolver/v1/health/live", cfg.Health.LivenessHandler)
	r.Get("/api/ai-resource-resolver/v1/health/ready", cfg.Health.ReadinessHandler)
	r.Get("/api/ai-resource-resolver/v1/health/startup", cfg.Health.ReadinessHandler)
}

// ResolverMountConfig carries the resolver HTTP dependencies.
type ResolverMountConfig struct {
	Service    *resolver.Service
	Authn      *authn.Validator
	Health     *Health
	RatePerSec int
	RateBurst  int
}

// handleResolve: raw-body hygiene (size limit, forbidden-field scan,
// strict unknown-field rejection) → service.Resolve → spec §3.1 envelope.
func (cfg *ResolverMountConfig) handleResolve(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFrom(r.Context())
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		writeResolverError(w, &resolver.ResolveError{
			Code: resolver.CodeInvalidRequirement, Status: http.StatusBadRequest,
			Msg: "Idempotency-Key header is required for resolve",
		}, rid)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, resolver.MaxRequestBytes))
	if err != nil {
		writeResolverError(w, &resolver.ResolveError{
			Code: resolver.CodeInvalidRequirement, Status: http.StatusBadRequest,
			Msg: "request body exceeds 256 KiB or is unreadable",
		}, rid)
		return
	}
	// 禁止字段检查 on the RAW body (routing step 2 — boundary scan; the
	// closed struct cannot carry undeclared keys)
	if err := resolver.CheckForbiddenFields(body); err != nil {
		writeResolverError(w, err.(*resolver.ResolveError), rid)
		return
	}
	var req resolver.Request
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields() // v1: 未声明字段默认拒绝（除 extensions）
	if err := dec.Decode(&req); err != nil {
		writeResolverError(w, &resolver.ResolveError{
			Code: resolver.CodeInvalidRequirement, Status: http.StatusBadRequest,
			Msg: "request body is not a valid resolve request: " + err.Error(),
		}, rid)
		return
	}

	id := middleware.IdentityFrom(r.Context())
	meta := resolver.CallerMeta{
		CallerRef:   id.Subject, // 身份来自已验证 token；请求体同名字段不得覆盖（specs §2）
		TenantRef:   id.TenantRef,
		Environment: r.Header.Get("X-Saoaf-Environment"),
		TraceID:     rid,
	}
	plan, _, err := cfg.Service.Resolve(r.Context(), &req, meta, idemKey)
	if err != nil {
		var re *resolver.ResolveError
		if errors.As(err, &re) {
			writeResolverError(w, re, rid)
			return
		}
		writeResolverError(w, &resolver.ResolveError{
			Code: resolver.CodeResolverUnavailable, Status: http.StatusServiceUnavailable,
			Msg: "resolve failed",
		}, rid)
		return
	}
	writeJSON(w, http.StatusOK, planResponse(plan))
}

// handleGetPlan: 只允许原 caller、受信审计角色读取（specs §3.2）。
func (cfg *ResolverMountConfig) handleGetPlan(w http.ResponseWriter, r *http.Request) {
	rid := middleware.RequestIDFrom(r.Context())
	id := middleware.IdentityFrom(r.Context())
	planID := chi.URLParam(r, "id")
	plan, err := cfg.Service.Plans.GetPlan(r.Context(), planID)
	if err != nil {
		if errors.Is(err, resolver.ErrNotFound) {
			writeResolverError(w, &resolver.ResolveError{
				Code: resolver.CodePlanNotFound, Status: http.StatusNotFound,
				Msg: "resource plan not found",
			}, rid)
			return
		}
		writeResolverError(w, &resolver.ResolveError{
			Code: resolver.CodeResolverUnavailable, Status: http.StatusServiceUnavailable,
			Msg: "plan store unavailable",
		}, rid)
		return
	}
	if plan.CallerRef != id.Subject && !id.HasScope("resolver.audit") {
		writeResolverError(w, &resolver.ResolveError{
			Code: resolver.CodeCallerNotAllowed, Status: http.StatusForbidden,
			Msg: "plan belongs to another caller",
		}, rid)
		return
	}
	writeJSON(w, http.StatusOK, planResponse(plan))
}

// planResponse maps the stored plan to the spec §3.1 response shape.
func planResponse(p *resolver.Plan) map[string]any {
	// 审计保留期内可查：TTL 过期后报告派生状态（不删除已生成 Plan）
	status := p.Status
	if status == resolver.StatusResolved {
		if exp, err := time.Parse(time.RFC3339Nano, p.ExpiresAt); err == nil &&
			time.Now().After(exp) {
			status = resolver.StatusExpired
		}
	}
	items := make([]map[string]any, 0, len(p.Items))
	for _, it := range p.Items {
		items = append(items, map[string]any{
			"requirement_id": it.RequirementID,
			"capability": map[string]any{
				"id":            it.CapabilityKey,
				"major_version": it.MajorVersion,
				"revision":      it.CapabilityRevision,
			},
			"binding": map[string]any{
				"id":       it.BindingKey,
				"revision": it.BindingRevision,
			},
			"provider": map[string]any{
				"id":               it.ProviderKey,
				"type":             it.ProviderType,
				"endpoint_ref":     it.EndpointRef,
				"contract_version": "",
			},
			"invocation": map[string]any{
				"protocol":          "HTTP_JSON",
				"profile_or_action": it.ProfileOrAction,
			},
			"reason_codes": it.ReasonCodes,
		})
	}
	return map[string]any{
		"resource_plan_id":     p.ID,
		"task_ref":             p.TaskRef,
		"status":               status,
		"created_at":           p.CreatedAt,
		"expires_at":           p.ExpiresAt,
		"decision_fingerprint": p.Fingerprint,
		"policy": map[string]any{
			"set_id":  p.PolicySetID,
			"version": p.PolicyVersion,
		},
		"items":    items,
		"trace_id": p.TraceID,
	}
}

// writeResolverError emits the unified error envelope (specs §3.1 错误信封
// — messages never leak candidate resources, SQL, or caller payloads).
func writeResolverError(w http.ResponseWriter, re *resolver.ResolveError, requestID string) {
	body := map[string]any{
		"error": map[string]any{
			"code":       re.Code,
			"message":    re.Msg,
			"request_id": requestID,
			"retryable":  re.Status >= 500 || re.Code == resolver.CodeRateLimited,
		},
	}
	if len(re.Details) > 0 {
		body["error"].(map[string]any)["details"] = re.Details
	}
	writeJSON(w, re.Status, body)
}
