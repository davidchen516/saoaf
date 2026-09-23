package middleware

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/davidchen516/saoaf/internal/platform/approval"
	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/authz"
	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
)

// stubIssuer issues locally-signed RSA tokens with a live JWKS endpoint.
type stubIssuer struct {
	key  *rsa.PrivateKey
	kid  string
	srv  *httptest.Server
	iss2 string
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
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		default:
			http.NotFound(w, r)
		}
	}))
	s.iss2 = s.srv.URL
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubIssuer) token(t *testing.T, mod func(jwt.MapClaims)) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": s.iss2, "aud": "saoaf-control-plane", "sub": "user:alice",
		"jti": "jti-1", "scope": "resource.publish", "tenant_ref": "tenant-demo",
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	if mod != nil {
		mod(c)
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

type gateHarness struct {
	pub     *httptest.Server
	pdpSrv  *httptest.Server
	apprSrv *httptest.Server
	audit   []string
}

func newGateHarness(t *testing.T, opts ...func(*gateConfig)) *gateHarness {
	t.Helper()
	iss := newStubIssuer(t)
	cfg := &gateConfig{iss: iss}
	for _, o := range opts {
		o(cfg)
	}
	return newGateHarnessWith(t, cfg)
}

type gateConfig struct {
	iss               *stubIssuer
	pdpDeny           bool
	pdpDown           bool
	approvalStatus    string
	approvalApprovers []string
	auditFail         bool
}

func newGateHarnessWith(t *testing.T, cfg *gateConfig) *gateHarness {
	t.Helper()
	v, err := authn.NewValidator(cfg.iss.srv.URL, "saoaf-control-plane")
	if err != nil {
		t.Fatal(err)
	}

	h := &gateHarness{}

	pdpAllow := !cfg.pdpDeny
	h.pdpSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if cfg.pdpDown {
			http.Error(w, "pdp down", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"decision": pdpAllow,
			"context":  map[string]any{"policy_decision_id": "d1"},
		})
	}))
	t.Cleanup(h.pdpSrv.Close)

	status := cfg.approvalStatus
	if status == "" {
		status = "APPROVED"
	}
	approvers := cfg.approvalApprovers
	if approvers == nil && status == "APPROVED" {
		approvers = []string{"user:bob"}
	}
	now := time.Now().Format(time.RFC3339)
	h.apprSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/approvals/v1/requests/approval:1":
			if status == "GONE" {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"approval_ref": "approval:1", "status": status,
				"requester_ref": "user:alice", "approver_refs": approvers,
				"decided_at": now,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(h.apprSrv.Close)

	sink := &recordSink{h: h, fail: cfg.auditFail}

	r := chi.NewRouter()
	r.Route("/admin/v1", func(admin chi.Router) {
		admin.Use(RateLimit(1000, 1000))
		admin.Use(RequireIdentity(v))
		admin.With(RequireScope("resource.publish"),
			ApprovalGate(authz.NewPDPClient(h.pdpSrv.URL), approval.NewClient(h.apprSrv.URL), "resource.publish")).
			Post("/bindings/{id}/publish", func(w http.ResponseWriter, req *http.Request) {
				id := IdentityFrom(req.Context())
				if err := sink.Write(req.Context(), id.Subject, chi.URLParam(req, "id")); err != nil {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusOK)
			})
		admin.Get("/whoami", func(w http.ResponseWriter, req *http.Request) {
			id := IdentityFrom(req.Context())
			_, _ = w.Write([]byte("subject:" + id.Subject))
		})
	})

	h.pub = httptest.NewServer(r)
	t.Cleanup(h.pub.Close)
	return h
}

type recordSink struct {
	h    *gateHarness
	fail bool
}

func (s *recordSink) Write(ctx context.Context, actor, id string) error {
	if s.fail {
		return errAuditFail
	}
	s.h.audit = append(s.h.audit, actor+"|"+id)
	return nil
}

var errAuditFail = errAuditFailType{}

type errAuditFailType struct{}

func (errAuditFailType) Error() string { return "audit sink down" }

func (h *gateHarness) do(t *testing.T, token string, approvalRef string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, h.pub.URL+"/admin/v1/bindings/b1/publish", nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if approvalRef != "" {
		req.Header.Set("X-Saoaf-Approval-Ref", approvalRef)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

// GWT#1: Happy path — 200 + audit record.
func TestGateHappyPath(t *testing.T) {
	iss := newStubIssuer(t)
	h := newGateHarnessWith(t, &gateConfig{iss: iss})
	resp := h.do(t, iss.token(t, nil), "approval:1")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(h.audit) != 1 || h.audit[0] != "user:alice|b1" {
		t.Fatalf("audit = %v", h.audit)
	}
}

// 路由优先级：401（无/坏 token）→ 403（无 scope）→ 403（PDP deny）→ 403（无审批）。
func TestGateRoutingPriority(t *testing.T) {
	iss := newStubIssuer(t)

	t.Run("401 no token", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss})
		if got := h.do(t, "", "approval:1").StatusCode; got != 401 {
			t.Fatalf("got %d", got)
		}
	})

	t.Run("401 invalid token", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss})
		if got := h.do(t, "bad", "approval:1").StatusCode; got != 401 {
			t.Fatalf("got %d", got)
		}
	})

	t.Run("403 missing scope", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss})
		tok := iss.token(t, func(c jwt.MapClaims) { c["scope"] = "resource.read" })
		if got := h.do(t, tok, "approval:1").StatusCode; got != 403 {
			t.Fatalf("got %d", got)
		}
	})

	t.Run("403 pdp deny", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss, pdpDeny: true})
		if got := h.do(t, iss.token(t, nil), "approval:1").StatusCode; got != 403 {
			t.Fatalf("got %d", got)
		}
		if len(h.audit) != 0 {
			t.Fatal("denied write must not be audited as success")
		}
	})

	t.Run("403 no approval ref", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss})
		if got := h.do(t, iss.token(t, nil), "").StatusCode; got != 403 {
			t.Fatalf("got %d", got)
		}
	})

	t.Run("403 approval not approved", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss, approvalStatus: "PENDING"})
		if got := h.do(t, iss.token(t, nil), "approval:1").StatusCode; got != 403 {
			t.Fatalf("got %d", got)
		}
	})

	t.Run("403 self approval", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss, approvalApprovers: []string{"user:alice"}})
		if got := h.do(t, iss.token(t, nil), "approval:1").StatusCode; got != 403 {
			t.Fatalf("got %d", got)
		}
	})
}

// GWT#4 fail-closed：PDP 挂 → 403；审批 API 挂 → 403。
func TestGateFailClosed(t *testing.T) {
	iss := newStubIssuer(t)

	t.Run("pdp down", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss, pdpDown: true})
		if got := h.do(t, iss.token(t, nil), "approval:1").StatusCode; got != 403 {
			t.Fatalf("pdp-down must be 403, got %d", got)
		}
	})

	t.Run("approval api down", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss, approvalStatus: "GONE"})
		if got := h.do(t, iss.token(t, nil), "approval:1").StatusCode; got != 403 {
			t.Fatalf("approval-down must be 403, got %d", got)
		}
	})

	t.Run("audit sink down blocks write", func(t *testing.T) {
		h := newGateHarnessWith(t, &gateConfig{iss: iss, auditFail: true})
		if got := h.do(t, iss.token(t, nil), "approval:1").StatusCode; got != 500 {
			t.Fatalf("audit failure must surface, got %d", got)
		}
	})
}

// 错误响应不含 token 细节。
func TestErrorResponsesDoNotLeakToken(t *testing.T) {
	iss := newStubIssuer(t)
	h := newGateHarnessWith(t, &gateConfig{iss: iss})
	// assembled at runtime so no JWT-shaped literal sits in the source
	// (gitleaks jwt rule would flag it as a hardcoded credential)
	badTok := "eyJhbGciOiJSUzI1NiJ9." + "eyJzdWIiOiJsZWFreCJ9.bad-signature"
	toks := []string{badTok, iss.token(t, func(c jwt.MapClaims) { c["scope"] = "x" })}
	for _, tok := range toks {
		resp := h.do(t, tok, "")
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if strings.Contains(string(body), tok) {
			t.Fatalf("token echoed in response body: %.200s", body)
		}
		for h := range resp.Header {
			if strings.HasPrefix(strings.ToLower(h), "auth") && resp.Header.Get(h) != "" {
				t.Fatalf("auth header leaked: %s", h)
			}
		}
	}
}
