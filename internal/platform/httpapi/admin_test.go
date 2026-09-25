package httpapi

// Admin publish handler tests (I16): the REAL domain publish runs behind
// the gate chain via the PublishBinding hook — sentinel → status mapping,
// frozen error envelopes, audit-on-success, and the routeRecorder guard
// against chi's silent route shadowing (review R3 P1-1: a re-registered
// publish route stripped the scope/PDP/approval gates).
import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"

	"github.com/davidchen516/saoaf/internal/platform/approval"
	"github.com/davidchen516/saoaf/internal/platform/audit"
	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/authz"
	"github.com/davidchen516/saoaf/internal/platform/middleware"
)

// stubIssuer issues locally-signed RSA tokens with a live JWKS endpoint
// (same pattern as the middleware gate tests).
type stubIssuer struct {
	key *rsa.PrivateKey
	kid string
	srv *httptest.Server
}

func newStubIssuer(t *testing.T) *stubIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &stubIssuer{key: key, kid: "k1"}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": s.srv.URL, "jwks_uri": s.srv.URL + "/keys"})
		case "/keys":
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
				"kty": "RSA", "kid": s.kid, "alg": "RS256",
				"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubIssuer) token(t *testing.T, scopes string) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": s.srv.URL, "aud": "saoaf-control-plane", "sub": "user:alice",
		"jti": "jti-1", "scope": scopes, "tenant_ref": "tenant-demo",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

type captureSink struct {
	entries []audit.Entry
	fail    bool
}

func (s *captureSink) Write(_ context.Context, e audit.Entry) error {
	if s.fail {
		return errors.New("audit sink down")
	}
	s.entries = append(s.entries, e)
	return nil
}

// publishTestServer mounts ONLY identity + the publish handler (the gate
// chain itself is proven by the middleware tests; these tests cover the
// handler behind it).
func publishTestServer(t *testing.T, sink audit.Sink, hook func(context.Context, PublishInput) (PublishResult, error)) (*httptest.Server, *stubIssuer) {
	t.Helper()
	iss := newStubIssuer(t)
	v, err := authn.NewValidator(iss.srv.URL, "saoaf-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	cfg := AdminConfig{Authn: v, Audit: sink, PublishBinding: hook}
	r := chi.NewRouter()
	r.Route("/admin/v1", func(sub chi.Router) {
		sub.Use(middleware.RequireIdentity(v))
		sub.Post("/bindings/{id}/publish", cfg.publishBinding)
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv, iss
}

func doPublish(srv *httptest.Server, token, ifMatch, approvalRef, idemKey, body string) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/v1/bindings/b1/publish", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ifMatch != "" {
		req.Header.Set("If-Match", ifMatch)
	}
	if approvalRef != "" {
		req.Header.Set("X-Saoaf-Approval-Ref", approvalRef)
	}
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err.Error()
	}
	defer resp.Body.Close()
	buf, _ := io.ReadAll(resp.Body) //nolint:errcheck
	return resp, string(buf)
}

func TestPublishHandlerNotConfiguredFailClosed(t *testing.T) {
	srv, iss := publishTestServer(t, audit.NoopSink{}, nil)
	resp, body := doPublish(srv, iss.token(t, "resource.publish"), "1", "approval:1", "", `{"change_reason":"r"}`)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unwired publish = %d %s, want 503 (fail-closed, no demo 200)", resp.StatusCode, body)
	}
	var env struct {
		ErrorCode string `json:"error_code"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatal(err)
	}
	if env.ErrorCode != "INTERNAL" || env.RequestID == "" {
		t.Fatalf("envelope = %+v (code INTERNAL + non-empty request_id required)", env)
	}
}

func TestPublishHandlerValidation(t *testing.T) {
	hook := func(context.Context, PublishInput) (PublishResult, error) {
		t.Fatal("hook must not run on validation failure")
		return PublishResult{}, nil
	}
	srv, iss := publishTestServer(t, audit.NoopSink{}, hook)

	// no expected revision at all
	resp, body := doPublish(srv, iss.token(t, "resource.publish"), "", "approval:1", "", `{"change_reason":"r"}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "VALIDATION_MISSING_REQUIRED") {
		t.Fatalf("no CAS key = %d %s, want 400 VALIDATION_MISSING_REQUIRED", resp.StatusCode, body)
	}
	// revision present but no change reason
	resp, body = doPublish(srv, iss.token(t, "resource.publish"), "1", "approval:1", "", `{}`)
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "change_reason") {
		t.Fatalf("no change_reason = %d %s, want 400", resp.StatusCode, body)
	}
	// body-only expected_revision (no If-Match) parses and reaches the hook
	srv2, iss2 := publishTestServer(t, audit.NoopSink{}, func(context.Context, PublishInput) (PublishResult, error) {
		return PublishResult{Revision: 2, State: "PUBLISHED"}, nil
	})
	resp, _ = doPublish(srv2, iss2.token(t, "resource.publish"), "", "approval:1", "", `{"expected_revision":1,"change_reason":"r"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("body-only expected_revision = %d, want 200 (reaches the hook)", resp.StatusCode)
	}
}

func TestPublishHandlerSuccessPassthrough(t *testing.T) {
	sink := &captureSink{}
	var got PublishInput
	hook := func(_ context.Context, in PublishInput) (PublishResult, error) {
		got = in
		return PublishResult{Revision: 2, State: "PUBLISHED"}, nil
	}
	srv, iss := publishTestServer(t, sink, hook)

	resp, body := doPublish(srv, iss.token(t, "resource.publish"), "1", "approval:1", "idem-1",
		`{"change_reason":"hub re-publish","ticket_ref":"T-9"}`)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish = %d %s", resp.StatusCode, body)
	}
	// the hook received the fully-correlated intent
	if got.BindingID != "b1" || got.ExpectedRevision != 1 ||
		got.ApprovalRef != "approval:1" || got.ChangeReason != "hub re-publish" ||
		got.IdempotencyKey != "idem-1" || got.TicketRef != "T-9" ||
		got.Actor != "user:alice" || got.TenantRef != "tenant-demo" || got.TraceID == "" {
		t.Fatalf("hook input = %+v", got)
	}
	// response carries the authoritative result + correlation
	var out struct {
		Status      string `json:"status"`
		Revision    int    `json:"revision"`
		State       string `json:"state"`
		DecisionRef string `json:"decision_ref"`
		RequestID   string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "published" || out.Revision != 2 || out.State != "PUBLISHED" ||
		out.DecisionRef != "approval:1" || out.RequestID == "" {
		t.Fatalf("response = %+v", out)
	}
	// audit records the applied operation with the decision reference
	if len(sink.entries) != 1 {
		t.Fatalf("audit entries = %d, want 1", len(sink.entries))
	}
	e := sink.entries[0]
	if e.Actor != "user:alice" || e.EntityID != "b1" || e.Operation != "PUBLISH" || e.DecisionRef != "approval:1" {
		t.Fatalf("audit entry = %+v", e)
	}

	// idempotent replay surfaces as already-published, no second audit row
	hook2 := func(context.Context, PublishInput) (PublishResult, error) {
		return PublishResult{Revision: 2, State: "PUBLISHED", IdempotentReplay: true}, nil
	}
	srv2, iss2 := publishTestServer(t, sink, hook2)
	resp, body = doPublish(srv2, iss2.token(t, "resource.publish"), "1", "approval:1", "idem-1",
		`{"change_reason":"hub re-publish"}`)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "already-published") {
		t.Fatalf("replay = %d %s, want 200 already-published", resp.StatusCode, body)
	}
	if len(sink.entries) != 2 {
		t.Fatalf("audit entries = %d, want 2 (replay audited once)", len(sink.entries))
	}
}

func TestPublishHandlerSentinelMapping(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantCode int
		wantEnv  string
	}{
		{"not-found", ErrPublishNotFound, http.StatusNotFound, "NOT_FOUND"},
		{"cas-conflict", ErrPublishConflict, http.StatusConflict, "CONFLICT"},
		{"invalid-transition", ErrPublishInvalidTransition, http.StatusConflict, "CONFLICT"},
		{"validation", ErrPublishValidation, http.StatusBadRequest, "VALIDATION_MISSING_REQUIRED"},
		{"unavailable", ErrPublishUnavailable, http.StatusServiceUnavailable, "INTERNAL"},
		{"unclassified", errors.New("pg exploded"), http.StatusInternalServerError, "INTERNAL"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hook := func(context.Context, PublishInput) (PublishResult, error) {
				return PublishResult{}, errors.Join(tc.err, errors.New(": detail"))
			}
			srv, iss := publishTestServer(t, audit.NoopSink{}, hook)
			resp, body := doPublish(srv, iss.token(t, "resource.publish"), "1", "approval:1", "",
				`{"change_reason":"r"}`)
			if resp.StatusCode != tc.wantCode {
				t.Fatalf("status = %d %s, want %d", resp.StatusCode, body, tc.wantCode)
			}
			if !strings.Contains(body, `"error_code":"`+tc.wantEnv+`"`) {
				t.Fatalf("envelope = %s, want error_code %s", body, tc.wantEnv)
			}
			if !strings.Contains(body, `"request_id":"`) || strings.Contains(body, `"request_id":""`) {
				t.Fatalf("request_id must be non-empty (frozen schema minLength 1): %s", body)
			}
		})
	}
}

// A successful publish whose audit write fails is surfaced as 500 (the
// operation applied — the message says so honestly).
func TestPublishHandlerAuditFailAfterPublish(t *testing.T) {
	sink := &captureSink{fail: true}
	hook := func(context.Context, PublishInput) (PublishResult, error) {
		return PublishResult{Revision: 2, State: "PUBLISHED"}, nil
	}
	srv, iss := publishTestServer(t, sink, hook)
	resp, body := doPublish(srv, iss.token(t, "resource.publish"), "1", "approval:1", "",
		`{"change_reason":"r"}`)
	if resp.StatusCode != http.StatusInternalServerError || !strings.Contains(body, "audit write failed") {
		t.Fatalf("audit fail = %d %s, want 500 with the applied-but-unaudited message", resp.StatusCode, body)
	}
}

// R3 P1-1 regression: re-registering the publish pattern inside the gated
// subtree must PANIC at startup — chi's overwrite semantics would silently
// strip the scope/PDP/approval chain off the route.
func TestAdminRouteShadowingPanics(t *testing.T) {
	iss := newStubIssuer(t)
	v, err := authn.NewValidator(iss.srv.URL, "saoaf-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	r := chi.NewRouter()
	defer func() {
		p := recover()
		if p == nil {
			t.Fatal("duplicate publish registration must panic (chi would silently shadow the gated handler)")
		}
		if msg, ok := p.(string); !ok || !strings.Contains(msg, "shadow") {
			t.Fatalf("panic = %v, want a shadowing diagnostic", p)
		}
	}()
	MountAdmin(r, AdminConfig{
		Authn: v, PDP: authz.NewPDPClient("http://stub.invalid"),
		Approvals: approval.NewClient("http://stub.invalid"), Audit: audit.NoopSink{},
		RatePerSec: 100, RateBurst: 100,
	}, func(admin chi.Router) {
		// the R3 P1-1 shape: the extension re-registers the gated route
		admin.Post("/bindings/{id}/publish", func(w http.ResponseWriter, r *http.Request) {})
	})
}

// Control: a benign extension (the hub read routes) mounts cleanly and the
// gated publish route SURVIVES — anonymous 401, missing-approval-ref 403
// (the gates run, not the extension's handler).
func TestAdminExtensionKeepsGatedPublish(t *testing.T) {
	iss := newStubIssuer(t)
	v, err := authn.NewValidator(iss.srv.URL, "saoaf-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	hook := func(context.Context, PublishInput) (PublishResult, error) {
		return PublishResult{}, errors.New("hook must not be reachable without the gates passing")
	}
	r := chi.NewRouter()
	MountAdmin(r, AdminConfig{
		Authn: v, PDP: authz.NewPDPClient("http://stub.invalid"),
		Approvals: approval.NewClient("http://stub.invalid"), Audit: audit.NoopSink{},
		RatePerSec: 100, RateBurst: 100, PublishBinding: hook,
	}, func(admin chi.Router) {
		admin.Get("/bindings", func(w http.ResponseWriter, r *http.Request) {
			WriteJSON(w, http.StatusOK, []any{})
		})
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	// the extension's read route is mounted inside the gated subtree
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/admin/v1/bindings", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous hub read = %d, want 401 (gated subtree)", resp.StatusCode)
	}

	// the gated publish route is NOT shadowed: authenticated but no
	// approval ref → 403 from the gate chain (the hook never runs)
	resp, body := doPublish(srv, iss.token(t, "resource.publish"), "1", "", "",
		`{"change_reason":"r"}`)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("publish without approval ref = %d %s, want 403 (gate chain intact)", resp.StatusCode, body)
	}
}
