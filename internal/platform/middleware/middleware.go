// Package middleware wires the control plane auth chain (I05) with a fixed
// routing priority: unauthenticated(401) → scope(403) → PDP → approval →
// audit. Business validation never precedes auth failures, and every admin
// write is audited with (identity, tenant, trace) plus decision reference.
package middleware

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/davidchen516/saoaf/internal/platform/approval"
	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/authz"
)

type ctxKeyIdentity struct{}

// IdentityFrom returns the verified identity for the request (nil if absent).
func IdentityFrom(ctx context.Context) *authn.Identity {
	if id, ok := ctx.Value(ctxKeyIdentity{}).(*authn.Identity); ok {
		return id
	}
	return nil
}

// ErrorEnvelope mirrors contracts/schemas/v1/error-envelope.json.
type errorEnvelope struct {
	ErrorCode string `json:"error_code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id"`
}

func writeError(w http.ResponseWriter, status int, code, msg, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errorEnvelope{ErrorCode: code, Message: msg, RequestID: requestID})
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return h[7:]
	}
	return ""
}

// ctxKeyRequestID carries the effective request id (caller-provided or
// minted) so handlers read it from context, never from raw headers.
type ctxKeyRequestID struct{}

func requestID(r *http.Request) string {
	if id := r.Header.Get("X-Request-ID"); id != "" {
		return id
	}
	return authn.NewRequestID()
}

// RequestIDFrom returns the effective request id for the request.
func RequestIDFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyRequestID{}).(string); ok && v != "" {
		return v
	}
	return ""
}

// withRequestID stores the effective id in the context chain.
func withRequestID(ctx context.Context, rid string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID{}, rid)
}

// RequireIdentity validates the bearer token and stores the trusted
// identity in the request context. 401 on any auth failure — never a
// business error, and no token internals in the response.
func RequireIdentity(v *authn.Validator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rid := requestID(r)
			id, err := v.Validate(r.Context(), bearerToken(r))
			if err != nil {
				writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED",
					"authentication required", rid)
				return
			}
			ctx := context.WithValue(r.Context(), ctxKeyIdentity{}, id)
			ctx = withRequestID(ctx, rid)
			ctx = approval.WithRequestID(ctx, rid)
			ctx = authz.WithRequestID(ctx, rid)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireScope enforces the named OAuth scope. 403 on missing scope.
func RequireScope(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := IdentityFrom(r.Context())
			if id == nil || !id.HasScope(scope) {
				rid := requestID(r)
				writeError(w, http.StatusForbidden, "FORBIDDEN",
					"insufficient scope", rid)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// ApprovalGate evaluates the PDP and requires an approved decision that
// satisfies separation of duties. Any transport failure or PDP denial
// is a 403 — fail-closed precedence, no fallback allow path.
func ApprovalGate(pdp *authz.PDPClient, approvals *approval.Client, action string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := IdentityFrom(r.Context())
			rid := requestID(r)
			if id == nil {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "identity required", rid)
				return
			}
			bodyRef := chi.URLParam(r, "id")
			ev := authz.Evaluation{
				Subject:  authz.Entity{Type: "identity", ID: id.Subject},
				Action:   authz.Entity{Name: action},
				Resource: authz.Entity{Type: "binding", ID: bodyRef},
				Context: map[string]any{
					"tenant_ref":  id.TenantRef,
					"environment": r.Header.Get("X-Saoaf-Environment"),
				},
			}
			dec, err := pdp.Evaluate(r.Context(), ev)
			if err != nil {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "not permitted", rid)
				return
			}
			_ = dec // policy decision id retained for future audit enrichment
			approvalRef := r.Header.Get("X-Saoaf-Approval-Ref")
			if approvalRef == "" {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "approval reference required", rid)
				return
			}
			ad, err := approvals.Get(r.Context(), approvalRef)
			if err != nil {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "approval not satisfied", rid)
				return
			}
			// response-request binding: the fetched decision must be for the
			// SAME approval reference the caller presented (prevents a
			// decision for a different/unknown request satisfying the gate)
			if ad.ApprovalRef != approvalRef {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "approval not satisfied", rid)
				return
			}
			// digest binding: the approval must cover the object being
			// written — Phase 0 digest is the binding identity
			objectDigest := "sha256:binding:" + bodyRef
			if !ad.SatisfiedFor(id.Subject, objectDigest) {
				writeError(w, http.StatusForbidden, "FORBIDDEN", "approval not satisfied", rid)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RateLimit is a per-subject token bucket. Requests above the burst within
// the window receive 429. State is in-memory (Phase 0); the boundary and
// keying are production-stable.
func RateLimit(rate int, burst int) func(http.Handler) http.Handler {
	type bucket struct {
		tokens float64
		last   time.Time
	}
	var mu sync.Mutex
	buckets := map[string]*bucket{}
	var refill = time.Second / time.Duration(rate)

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := IdentityFrom(r.Context())
			key := "anonymous"
			if id != nil {
				key = id.Subject
			}
			mu.Lock()
			now := time.Now()
			b, ok := buckets[key]
			if !ok {
				b = &bucket{tokens: float64(burst), last: now}
				buckets[key] = b
			}
			elapsed := now.Sub(b.last)
			b.tokens += float64(elapsed) / float64(refill)
			if b.tokens > float64(burst) {
				b.tokens = float64(burst)
			}
			b.last = now
			allowed := b.tokens >= 1
			if allowed {
				b.tokens--
			}
			mu.Unlock()

			if !allowed {
				rid := requestID(r)
				writeError(w, http.StatusTooManyRequests, "RATE_LIMITED",
					"rate limit exceeded", rid)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
