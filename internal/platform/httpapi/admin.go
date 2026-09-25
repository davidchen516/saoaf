// Protected admin endpoints (I05 demo wiring; replaced by the real Admin
// API in I06+). The auth chain order is fixed by the issue: identity(401)
// → scope(403) → PDP → approval → audit.
package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/davidchen516/saoaf/internal/platform/approval"
	"github.com/davidchen516/saoaf/internal/platform/audit"
	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/authz"
	"github.com/davidchen516/saoaf/internal/platform/middleware"
)

// AdminConfig wires the I05 adapters into the admin demo endpoints.
type AdminConfig struct {
	Authn      *authn.Validator
	PDP        *authz.PDPClient
	Approvals  *approval.Client
	Audit      audit.Sink
	RatePerSec int
	RateBurst  int
}

// MountAdmin wires /admin/v1 under the fixed auth chain.
func MountAdmin(r chi.Router, cfg AdminConfig, extend ...func(admin chi.Router)) {
	r.Route("/admin/v1", func(admin chi.Router) {
		// identity FIRST so the rate limiter buckets per-subject (an
		// anonymous flood must not starve authenticated admins; anonymous
		// callers are rejected before any bucket is consumed)
		admin.Use(middleware.RequireIdentity(cfg.Authn))
		admin.Use(middleware.RateLimit(cfg.RatePerSec, cfg.RateBurst))

		// whoami: any authenticated identity (diagnostic + smoke test)
		admin.Get("/whoami", func(w http.ResponseWriter, req *http.Request) {
			id := middleware.IdentityFrom(req.Context())
			WriteJSON(w, http.StatusOK, map[string]any{
				"subject":    id.Subject,
				"tenant_ref": id.TenantRef,
				"scopes":     id.Scopes,
			})
		})

		// bindings publish: high-risk write demo — scope → PDP → approval → audit
		admin.With(
			middleware.RequireScope("resource.publish"),
			middleware.ApprovalGate(cfg.PDP, cfg.Approvals, "resource.publish"),
		).Post("/bindings/{id}/publish", func(w http.ResponseWriter, req *http.Request) {
			id := middleware.IdentityFrom(req.Context())
			approvalRef := req.Header.Get("X-Saoaf-Approval-Ref")
			bindingID := chi.URLParam(req, "id")
			traceID := middleware.RequestIDFrom(req.Context())

			if err := cfg.Audit.Write(req.Context(), audit.Entry{
				TenantRef:   id.TenantRef,
				Actor:       id.Subject,
				TraceID:     traceID,
				EntityKind:  "binding",
				EntityID:    bindingID,
				Operation:   "PUBLISH",
				DecisionRef: approvalRef,
			}); err != nil {
				WriteJSON(w, http.StatusInternalServerError, map[string]any{
					"error_code": "INTERNAL",
					"message":    "audit write failed",
					"request_id": traceID,
				})
				return
			}
			WriteJSON(w, http.StatusOK, map[string]any{
				"status":       "published",
				"binding_id":   bindingID,
				"decision_ref": approvalRef,
				"request_id":   traceID,
			})
		})
		// I16+ extension routes mount INSIDE the gated subtree
		for _, ext := range extend {
			ext(admin)
		}
	})
}
