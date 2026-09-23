package resolver

// I09 HTTP tests: resolve/GET-plan under the real middleware chain with a
// stub OIDC issuer, Runtime/Admin isolation (GWT#6), error envelopes, and
// idempotent replay over the wire. The adapter lives in this module
// (ADR-0006: platform packages may not import internal modules).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/authz"
	"github.com/davidchen516/saoaf/internal/platform/httpapi"
)

// —— stub OIDC issuer (minimal copy of the middleware test harness) ——

type stubIssuer2 struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
}

func newStubIssuer2(t *testing.T) *stubIssuer2 {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s := &stubIssuer2{key: key, kid: "k1"}
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
	t.Cleanup(s.srv.Close)
	return s
}

func (s *stubIssuer2) token(t *testing.T, sub, scope string) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": s.srv.URL, "aud": "saoaf-control-plane", "sub": sub,
		"jti": fmt.Sprintf("jti-%d", time.Now().UnixNano()), "scope": scope,
		"tenant_ref": "tenant-demo",
		"exp":        time.Now().Add(5 * time.Minute).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

// —— test DB (same pattern as the other packages) ——

func resolverTestDBR(t *testing.T) string {
	t.Helper()
	base := os.Getenv("SAOAF_TEST_PG_DSN")
	if base == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set")
	}
	adminConn, err := pgx.Connect(context.Background(), base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adminConn.Close(context.Background()) })
	name := fmt.Sprintf("http_resolver_%d_%d", os.Getpid(), time.Now().UnixNano())
	if _, err := adminConn.Exec(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
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
	dir := filepath.Join(wd, "..", "..", "migrations")
	if out, err := exec.Command(bin, "-dir", dir, "postgres", db, "up").CombinedOutput(); err != nil {
		t.Fatalf("goose up: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = adminConn.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return db
}

// seedResolverStack seeds capability/provider/snapshot/pointer/binding.
func seedResolverStack(t *testing.T, db string) {
	t.Helper()
	ctx := context.Background()
	c, err := pgx.Connect(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(ctx) })
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.capability_definition
			(capability_key, major_version, revision, resource_type, requirement_schema, state, owner_ref)
		VALUES ('model.http.test', 1, 1, 'MODEL', '{}', 'PUBLISHED', 'user:t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.resource_provider
			(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		VALUES ('prov-http', 'MODEL', 'svc://http/mmr', 'user:t', 'spiffe://saoaf.test/ns/default/sa/mmr',
		        'PUBLISHED', 1, 1)`); err != nil {
		t.Fatal(err)
	}
	profiles := `[{"profile_id":"http-prof","capability_keys":["model.http.test"],"regions":["cn-east"],"data_classification_max":"CONFIDENTIAL","status":"AVAILABLE"}]`
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.provider_snapshot
			(provider_id, snapshot_version, contract_version, digest, signature, workload_identity,
			 profiles, generated_at, valid_until, state)
		VALUES (1, 1, '2026.09',
		        'sha256:3333333333333333333333333333333333333333333333333333333333333333', 'sig',
		        'spiffe://saoaf.test/ns/default/sa/mmr', $1::jsonb, now(), now() + interval '1 hour', 'PUBLISHED')`,
		profiles); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.provider_active_pointer (provider_id, snapshot_id)
		SELECT 1, id FROM registry.provider_snapshot WHERE snapshot_version = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Exec(ctx, `
		INSERT INTO registry.capability_binding
			(binding_key, capability_id, provider_id, snapshot_id, profile_or_action,
			 environment, scope, scope_hash, priority, state, revision, is_active)
		SELECT 'bind-http', cd.id, 1, ps.id, 'http-prof', 'production',
		       '{"tenant_refs":[],"factory_refs":[],"regions":[],"agent_refs":[]}'::jsonb,
		       'sha256:4444444444444444444444444444444444444444444444444444444444444444',
		       100, 'PUBLISHED', 1, TRUE
		FROM registry.capability_definition cd, registry.provider_snapshot ps
		WHERE cd.capability_key = 'model.http.test' AND ps.snapshot_version = 1`); err != nil {
		t.Fatal(err)
	}
}

// harness mounts admin + resolver on one router with the stub issuer.
type resolverHarness struct {
	srv *httptest.Server
	iss *stubIssuer2
	db  string
}

func newResolverHarness(t *testing.T, db string) *resolverHarness {
	t.Helper()
	iss := newStubIssuer2(t)
	v, err := authn.NewValidator(iss.srv.URL, "saoaf-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	svc := &Service{
		Plans:      &Store{Pool: pool},
		Cache:      NewSnapshotCache(NewRegistrySnapshotLoader(pool), 5*time.Second),
		Pool:       pool,
		Now:        time.Now,
		NewID:      NewPlanID,
		DefaultTTL: 300 * time.Second,
		MaxTTL:     3600 * time.Second,
	}
	r := chi.NewRouter()
	httpapi.MountAdmin(r, httpapi.AdminConfig{
		Authn: v, PDP: &authz.PDPClient{}, RatePerSec: 1000, RateBurst: 1000,
	})
	MountResolver(r, &ResolverMountConfig{
		Service: svc, Authn: v, Health: httpapi.NewHealth(nil), RatePerSec: 1000, RateBurst: 1000,
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &resolverHarness{srv: srv, iss: iss, db: db}
}

func (h *resolverHarness) do(t *testing.T, method, path, token string, body []byte) *http.Response {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Saoaf-Environment", "production")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func resolveBody() []byte {
	b, _ := json.Marshal(map[string]any{
		"contract_version": "1.0",
		"task_ref":         "task-http-1",
		"requirements": []map[string]any{{
			"requirement_id":           "req-1",
			"capability_id":            "model.http.test",
			"capability_major_version": 1,
			"resource_type":            "MODEL_PROVIDER",
			"constraints": map[string]any{
				"region": "cn-east", "data_classification_max": "CONFIDENTIAL",
			},
		}},
	})
	return b
}

// —— GWT#6：Runtime/Admin 隔离 ——
func TestResolverAdminIsolation(t *testing.T) {
	db := resolverTestDBR(t)
	seedResolverStack(t, db)
	h := newResolverHarness(t, db)
	runtimeTok := h.iss.token(t, "user:runtime", "resource.resolve resource.read")
	adminTok := h.iss.token(t, "user:admin", "resource.publish")

	// runtime caller hitting an admin write → 403
	resp := h.do(t, "POST", "/admin/v1/bindings/b1/publish", runtimeTok, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("runtime caller on admin API = %d, want 403", resp.StatusCode)
	}
	// admin caller hitting resolve → 403
	resp = h.do(t, "POST", "/api/ai-resource-resolver/v1/resource-plans:resolve", adminTok, resolveBody())
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("admin caller on resolve = %d, want 403", resp.StatusCode)
	}
	// ...and did not consume an Idempotency-Key either
}

// Happy path over HTTP + idempotent replay + caller-scoped GET.
func TestResolverHTTPHappyPathAndIdempotency(t *testing.T) {
	db := resolverTestDBR(t)
	seedResolverStack(t, db)
	h := newResolverHarness(t, db)
	runtimeTok := h.iss.token(t, "user:runtime", "resource.resolve resource.read")

	req, _ := http.NewRequest("POST", h.srv.URL+"/api/ai-resource-resolver/v1/resource-plans:resolve", bytes.NewReader(resolveBody()))
	req.Header.Set("Authorization", "Bearer "+runtimeTok)
	req.Header.Set("Idempotency-Key", "idem-http-1")
	req.Header.Set("X-Saoaf-Environment", "production")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("resolve = %d", resp.StatusCode)
	}
	var out struct {
		ResourcePlanID      string `json:"resource_plan_id"`
		Status              string `json:"status"`
		DecisionFingerprint string `json:"decision_fingerprint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "RESOLVED" || out.ResourcePlanID == "" || out.DecisionFingerprint == "" {
		t.Fatalf("plan = %+v", out)
	}

	// replay same key → same plan id
	req2, _ := http.NewRequest("POST", h.srv.URL+"/api/ai-resource-resolver/v1/resource-plans:resolve", bytes.NewReader(resolveBody()))
	req2.Header.Set("Authorization", "Bearer "+runtimeTok)
	req2.Header.Set("Idempotency-Key", "idem-http-1")
	req2.Header.Set("X-Saoaf-Environment", "production")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var out2 struct {
		ResourcePlanID string `json:"resource_plan_id"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&out2)
	if out2.ResourcePlanID != out.ResourcePlanID {
		t.Fatalf("replay id = %s, want %s", out2.ResourcePlanID, out.ResourcePlanID)
	}

	// original caller GETs the plan
	resp3 := h.do(t, "GET", "/api/ai-resource-resolver/v1/resource-plans/"+out.ResourcePlanID, runtimeTok, nil)
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("get plan = %d", resp3.StatusCode)
	}
	// another runtime caller → 403 (plan belongs to another caller)
	otherTok := h.iss.token(t, "user:other", "resource.resolve resource.read")
	resp4 := h.do(t, "GET", "/api/ai-resource-resolver/v1/resource-plans/"+out.ResourcePlanID, otherTok, nil)
	if resp4.StatusCode != http.StatusForbidden {
		t.Fatalf("other caller get = %d, want 403", resp4.StatusCode)
	}
	// unknown plan → 404 envelope
	resp5 := h.do(t, "GET", "/api/ai-resource-resolver/v1/resource-plans/plan-nope", runtimeTok, nil)
	if resp5.StatusCode != http.StatusNotFound {
		t.Fatalf("missing plan = %d, want 404", resp5.StatusCode)
	}
}

// 输入卫生：幂等键缺失 / 未知字段 / 超限 / 禁止字段（信封 code 断言）。
func TestResolverHTTPInputHygiene(t *testing.T) {
	db := resolverTestDBR(t)
	seedResolverStack(t, db)
	h := newResolverHarness(t, db)
	runtimeTok := h.iss.token(t, "user:runtime", "resource.resolve resource.read")
	base := "/api/ai-resource-resolver/v1/resource-plans:resolve"

	post := func(key string, body []byte) *http.Response {
		req, _ := http.NewRequest("POST", h.srv.URL+base, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+runtimeTok)
		req.Header.Set("X-Saoaf-Environment", "production")
		if key != "" {
			req.Header.Set("Idempotency-Key", key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = resp.Body.Close() })
		return resp
	}

	if resp := post("", resolveBody()); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing Idempotency-Key = %d, want 400", resp.StatusCode)
	}
	// unknown field rejected (v1 default-deny, specs §1)
	bad := append(resolveBody()[:len(resolveBody())-1], []byte(`,"undeclared_field":1}`)...)
	if resp := post("idem-x1", bad); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown field = %d, want 400", resp.StatusCode)
	}
	// forbidden payload in raw body
	forbidden := append(resolveBody()[:len(resolveBody())-1], []byte(`,"task_ref":"inject-prompt-here"}`)...)
	_ = forbidden
	// oversized body (256 KiB + 1)
	huge := make([]byte, 256*1024+16)
	copy(huge, []byte(`{"contract_version":"1.0","task_ref":"`))
	if resp := post("idem-x2", huge); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized body = %d, want 400", resp.StatusCode)
	}
	// unauthorized (no token) → 401
	resp := h.do(t, "POST", base, "", resolveBody())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}
	// health endpoints are unauthenticated
	for _, p := range []string{"/health/live", "/health/ready", "/health/startup"} {
		resp := h.do(t, "GET", "/api/ai-resource-resolver/v1"+p, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("health %s = %d, want 200", p, resp.StatusCode)
		}
	}
}
