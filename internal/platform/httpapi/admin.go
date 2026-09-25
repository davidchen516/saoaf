// Protected admin endpoints (I05 wiring; the publish route executes the
// REAL domain publish since I16). The auth chain order is fixed by the
// issue: identity(401) → scope(403) → PDP → approval → audit.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/davidchen516/saoaf/internal/platform/approval"
	"github.com/davidchen516/saoaf/internal/platform/audit"
	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/authz"
	"github.com/davidchen516/saoaf/internal/platform/middleware"
)

// Publish sentinel errors — the composition root maps the binding module's
// domain sentinels onto these (ADR-0006: this platform package cannot import
// domain modules; the function hook is the boundary).
var (
	// ErrPublishNotFound: the binding key does not exist (→ 404).
	ErrPublishNotFound = errors.New("publish: binding not found")
	// ErrPublishConflict: CAS/state/scope conflict — nothing was overwritten
	// (→ 409).
	ErrPublishConflict = errors.New("publish: conflict")
	// ErrPublishInvalidTransition: the current state cannot be published
	// (SUSPENDED must Resume, RETIRED is terminal) (→ 409).
	ErrPublishInvalidTransition = errors.New("publish: invalid state transition")
	// ErrPublishValidation: the domain publish gate rejected the request
	// (→ 400).
	ErrPublishValidation = errors.New("publish: validation rejected")
	// ErrPublishUnavailable: the store is up but refuses the write
	// fail-closed (e.g. the I10 event-backlog gate) (→ 503).
	ErrPublishUnavailable = errors.New("publish: store unavailable")
)

// PublishInput carries the parsed publish intent from the admin chain.
type PublishInput struct {
	BindingID        string
	ExpectedRevision int
	ApprovalRef      string
	ChangeReason     string
	TicketRef        string
	IdempotencyKey   string
	Actor            string
	TenantRef        string
	TraceID          string
}

// PublishResult reports the authoritative post-publish state.
type PublishResult struct {
	Revision         int
	State            string
	IdempotentReplay bool
}

// AdminConfig wires the I05 adapters into the admin endpoints.
type AdminConfig struct {
	Authn      *authn.Validator
	PDP        *authz.PDPClient
	Approvals  *approval.Client
	Audit      audit.Sink
	RatePerSec int
	RateBurst  int
	// PublishBinding executes the real domain publish. It runs INSIDE the
	// scope → PDP → approval chain, after which this handler writes the
	// audit entry. Wired by the composition root to the binding module
	// (cmd/control-plane-api). Nil keeps the route fail-closed (503): the
	// pre-I16 demo handler answered 200 without publishing anything.
	PublishBinding func(ctx context.Context, in PublishInput) (PublishResult, error)
}

// MountAdmin wires /admin/v1 under the fixed auth chain. Extensions (I16
// hub) mount INSIDE the identity-gated subtree through the routeRecorder —
// a duplicate method+pattern registration panics instead of silently
// shadowing the earlier handler (review R3 P1-1: chi's overwrite semantics
// stripped the scope/PDP/approval gates off the publish route).
func MountAdmin(r chi.Router, cfg AdminConfig, extend ...func(admin chi.Router)) {
	r.Route("/admin/v1", func(sub chi.Router) {
		admin := newRouteRecorder(sub)
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

		// bindings publish: scope → PDP → approval → REAL publish → audit
		admin.With(
			middleware.RequireScope("resource.publish"),
			middleware.ApprovalGate(cfg.PDP, cfg.Approvals, "resource.publish"),
		).Post("/bindings/{id}/publish", cfg.publishBinding)
		// I16+ extension routes mount INSIDE the gated subtree
		for _, ext := range extend {
			ext(admin)
		}
	})
}

// publishBinding executes the real publish behind the gate chain. Errors
// carry the frozen envelope; conflicts are 409s (never overwrites).
func (cfg AdminConfig) publishBinding(w http.ResponseWriter, req *http.Request) {
	id := middleware.IdentityFrom(req.Context())
	approvalRef := req.Header.Get("X-Saoaf-Approval-Ref")
	bindingID := chi.URLParam(req, "id")
	traceID := middleware.RequestIDFrom(req.Context())

	if cfg.PublishBinding == nil {
		WriteErr(w, req, http.StatusServiceUnavailable, "INTERNAL",
			"publish store not configured (fail-closed)")
		return
	}

	// expected_revision from If-Match (UI) or body; change_reason/ticket
	// from the body
	expected := req.Header.Get("If-Match")
	var body struct {
		ExpectedRevision *int   `json:"expected_revision"`
		ChangeReason     string `json:"change_reason"`
		TicketRef        string `json:"ticket_ref"`
	}
	if req.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, req.Body, 64*1024)).Decode(&body) //nolint:errcheck // absence handled by the field checks below
	}
	if expected == "" && body.ExpectedRevision != nil {
		expected = strconv.Itoa(*body.ExpectedRevision)
	}
	expRev, err := strconv.Atoi(expected)
	if err != nil || expRev < 1 {
		WriteErr(w, req, http.StatusBadRequest, "VALIDATION_MISSING_REQUIRED",
			"If-Match or expected_revision required (CAS)")
		return
	}
	if body.ChangeReason == "" {
		WriteErr(w, req, http.StatusBadRequest, "VALIDATION_MISSING_REQUIRED",
			"change_reason required (change record)")
		return
	}

	res, perr := cfg.PublishBinding(req.Context(), PublishInput{
		BindingID:        bindingID,
		ExpectedRevision: expRev,
		ApprovalRef:      approvalRef,
		ChangeReason:     body.ChangeReason,
		TicketRef:        body.TicketRef,
		IdempotencyKey:   req.Header.Get("Idempotency-Key"),
		Actor:            id.Subject,
		TenantRef:        id.TenantRef,
		TraceID:          traceID,
	})
	if perr != nil {
		switch {
		case errors.Is(perr, ErrPublishNotFound):
			WriteErr(w, req, http.StatusNotFound, "NOT_FOUND", perr.Error())
		case errors.Is(perr, ErrPublishConflict), errors.Is(perr, ErrPublishInvalidTransition):
			WriteErr(w, req, http.StatusConflict, "CONFLICT", perr.Error())
		case errors.Is(perr, ErrPublishValidation):
			WriteErr(w, req, http.StatusBadRequest, "VALIDATION_MISSING_REQUIRED", perr.Error())
		case errors.Is(perr, ErrPublishUnavailable):
			WriteErr(w, req, http.StatusServiceUnavailable, "INTERNAL", perr.Error())
		default:
			WriteErr(w, req, http.StatusInternalServerError, "INTERNAL", "publish failed")
		}
		return
	}

	// audit the applied operation (decision_ref correlates the approval;
	// the domain change_record is written transactionally by the store)
	if err := cfg.Audit.Write(req.Context(), audit.Entry{
		TenantRef:   id.TenantRef,
		Actor:       id.Subject,
		TraceID:     traceID,
		EntityKind:  "binding",
		EntityID:    bindingID,
		Operation:   "PUBLISH",
		DecisionRef: approvalRef,
	}); err != nil {
		WriteErr(w, req, http.StatusInternalServerError, "INTERNAL",
			"publish applied but audit write failed")
		return
	}
	status := "published"
	if res.IdempotentReplay {
		status = "already-published"
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"status":       status,
		"binding_id":   bindingID,
		"revision":     res.Revision,
		"state":        res.State,
		"decision_ref": approvalRef,
		"request_id":   traceID,
	})
}

// WriteErr emits the FROZEN error envelope contract:
// flat {error_code, message, request_id} (contracts/schemas/v1/error-envelope.json).
// An empty request_id falls back to a UnixNano string — the schema requires
// minLength 1 (review R2 P2).
func WriteErr(w http.ResponseWriter, req *http.Request, status int, code, msg string) {
	requestID := middleware.RequestIDFrom(req.Context())
	if requestID == "" {
		requestID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if len(msg) > 512 {
		msg = msg[:512] // schema maxLength: 512
	}
	WriteJSON(w, status, map[string]any{
		"error_code": code,
		"message":    msg,
		"request_id": requestID,
	})
}

// routeRecorder guards against chi's silent route shadowing: registering the
// same method+pattern twice on one router does NOT panic in chi v5 — the
// later registration wins and the earlier handler (with its middleware
// chain) never runs (review R3 P1-1: a re-registered publish route stripped
// the scope/PDP/approval gates). Recording registrations and panicking on
// duplicates turns shadowing into a startup failure instead of a security
// regression.
type routeRecorder struct {
	chi.Router
	seen map[string]string
}

func newRouteRecorder(r chi.Router) *routeRecorder {
	return &routeRecorder{Router: r, seen: map[string]string{}}
}

func (rec *routeRecorder) record(method, pattern string) {
	key := method + " " + pattern
	if _, dup := rec.seen[key]; dup {
		panic("saoaf: duplicate route registration '" + key +
			"' would silently shadow the earlier handler (chi overwrite semantics)")
	}
	rec.seen[key] = key
}

// With must keep recording through the derived inline router — gate-wrapped
// registrations (e.g. the publish route) are the ones shadowing protects.
func (rec *routeRecorder) With(mw ...func(http.Handler) http.Handler) chi.Router {
	return &routeRecorder{Router: rec.Router.With(mw...), seen: rec.seen}
}

func (rec *routeRecorder) Get(pattern string, h http.HandlerFunc) {
	rec.record("GET", pattern)
	rec.Router.Get(pattern, h)
}

func (rec *routeRecorder) Post(pattern string, h http.HandlerFunc) {
	rec.record("POST", pattern)
	rec.Router.Post(pattern, h)
}

func (rec *routeRecorder) Put(pattern string, h http.HandlerFunc) {
	rec.record("PUT", pattern)
	rec.Router.Put(pattern, h)
}

func (rec *routeRecorder) Patch(pattern string, h http.HandlerFunc) {
	rec.record("PATCH", pattern)
	rec.Router.Patch(pattern, h)
}

func (rec *routeRecorder) Delete(pattern string, h http.HandlerFunc) {
	rec.record("DELETE", pattern)
	rec.Router.Delete(pattern, h)
}

func (rec *routeRecorder) Head(pattern string, h http.HandlerFunc) {
	rec.record("HEAD", pattern)
	rec.Router.Head(pattern, h)
}

func (rec *routeRecorder) Options(pattern string, h http.HandlerFunc) {
	rec.record("OPTIONS", pattern)
	rec.Router.Options(pattern, h)
}

func (rec *routeRecorder) Trace(pattern string, h http.HandlerFunc) {
	rec.record("TRACE", pattern)
	rec.Router.Trace(pattern, h)
}

func (rec *routeRecorder) Connect(pattern string, h http.HandlerFunc) {
	rec.record("CONNECT", pattern)
	rec.Router.Connect(pattern, h)
}

func (rec *routeRecorder) Method(method, pattern string, h http.Handler) {
	rec.record(method, pattern)
	rec.Router.Method(method, pattern, h)
}

func (rec *routeRecorder) HandleFunc(pattern string, h http.HandlerFunc) {
	rec.record("HANDLEFUNC", pattern)
	rec.Router.HandleFunc(pattern, h)
}

func (rec *routeRecorder) Handle(pattern string, h http.Handler) {
	rec.record("HANDLE", pattern)
	rec.Router.Handle(pattern, h)
}
