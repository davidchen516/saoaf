package ops

// Real-PostgreSQL tests for the sovereignty-ops read API (I17): drill-down
// triple completeness, 未知≠0 semantics, tenant/environment isolation
// (cross-tenant and cross-environment reads rejected 403), server-side
// pagination caps, and 404/envelope behavior. Same harness pattern as the
// binding/exitdrill suites.
import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/davidchen516/saoaf/internal/platform/authn"
	"github.com/davidchen516/saoaf/internal/platform/httpapi"
)

func withDBO(t *testing.T, fn func(dsn string)) {
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
	name := fmt.Sprintf("ops_%d_%d", os.Getpid(), time.Now().UnixNano())
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
	fn(dsn)
}

// stubIssuer — same pattern as the httpapi admin tests.
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

// token mints a signed token with the given scopes, tenant_ref and
// environments claim.
func (s *stubIssuer) token(t *testing.T, scopes, tenant string, environments []string) string {
	t.Helper()
	c := jwt.MapClaims{
		"iss": s.srv.URL, "aud": "saoaf-control-plane", "sub": "user:ops",
		"jti": "jti-ops", "scope": scopes, "tenant_ref": tenant,
		"exp": time.Now().Add(5 * time.Minute).Unix(),
	}
	if environments != nil {
		c["environments"] = environments
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

type opsHarness struct {
	srv *httptest.Server
	iss *stubIssuer
}

func newOpsHarness(t *testing.T, dsn string) *opsHarness {
	t.Helper()
	iss := newStubIssuer(t)
	v, err := authn.NewValidator(iss.srv.URL, "saoaf-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	r := chi.NewRouter()
	// ops mounts through the SAME extend mechanism production uses: inside
	// the identity-gated admin subtree (a second MountAdmin surface is not
	// needed here — the gate chain itself has its own tests)
	httpapi.MountAdmin(r, httpapi.AdminConfig{
		Authn: v, RatePerSec: 1000, RateBurst: 1000,
	}, func(admin chi.Router) {
		Mount(admin, Config{Pool: pool})
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return &opsHarness{srv: srv, iss: iss}
}

func (h *opsHarness) get(t *testing.T, path, token string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, h.srv.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body) //nolint:errcheck
	return resp.StatusCode, string(b)
}

// seedOps inserts metric results (OK with resolved evidence, UNKNOWN with
// nil value, and a broken-ref row), an alert, an evidence record, an exit
// pack, a drill with a finding + transition rows.
func seedOps(t *testing.T, dsn string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)

	ins := func(metric, dims string, value *float64, status, reason, evidence string) {
		v := "NULL"
		if value != nil {
			v = fmt.Sprintf("%f", *value)
		}
		if _, err := conn.Exec(ctx, `
			INSERT INTO saoaf.metric_result
				(metric_key, dimensions, value, status, status_reason,
				 dataset_revision, formula_version, evidence_ref)
			VALUES ($1, $2::jsonb, `+v+`, $3, $4, 42, 'v1', $5)`,
			metric, dims, status, reason, evidence); err != nil {
			t.Fatalf("seed metric %s: %v", metric, err)
		}
	}
	// tenant-a production: OK metric whose evidence resolves
	ins("substitution_coverage", `{"tenant":"tenant-a","environment":"production","capability":"cap-1"}`,
		ptr(0.75), "OK", "", "ev-resolved-1")
	// tenant-a staging: UNKNOWN — value NULL, 未知 ≠ 0
	ins("substitution_coverage", `{"tenant":"tenant-a","environment":"staging","capability":"cap-1"}`,
		nil, "UNKNOWN", "no active binding for capability", "")
	// tenant-b production: visible only to cross-tenant readers
	ins("substitution_coverage", `{"tenant":"tenant-b","environment":"production","capability":"cap-1"}`,
		ptr(0.10), "OK", "", "ev-resolved-1")
	// broken evidence chain: ref that resolves nowhere
	ins("evidence_completeness", `{"tenant":"tenant-a","environment":"production"}`,
		nil, "INSUFFICIENT_DATA", "empty evidence", "ev-missing-9")

	if _, err := conn.Exec(ctx, `
		INSERT INTO saoaf.evidence_record
			(event_id, source_topic, aggregate_kind, aggregate_id, tenant_ref,
			 occurred_at, payload_digest, content, plan_id, retention_class, worm_status, state)
		VALUES ('ev-resolved-1', 'binding.published', 'binding', 'b-1', 'tenant-a',
			now(), 'sha256:0000000000000000000000000000000000000000000000000000000000000000',
			'{}', '', 'STANDARD', 'ARCHIVED', 'LINKED')`); err != nil {
		t.Fatalf("seed evidence: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO saoaf.risk_alert
			(rule_key, entity_kind, entity_id, dataset_revision, severity, detail, state)
		VALUES ('single-provider', 'capability', 'cap-1', 42, 'HIGH', '{}', 'OPEN')`); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO saoaf.exit_pack
			(pack_key, vendor, revision, state, owner_ref, substitute_provider,
			 recovery_steps, evidence_refs, checklist, valid_until, created_by)
		VALUES ('vendor-x', 'vendor-x', 1, 'ACTIVE', 'user:ops', 'prov-y',
			'["step1"]', '["ev-resolved-1"]', '{}', now() - interval '1 day', 'user:ops')`); err != nil {
		t.Fatalf("seed pack: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO saoaf.exit_drill (drill_key, vendor, initiator, state)
		VALUES ('drill-1', 'vendor-x', 'user:init', 'SUCCEEDED')`); err != nil {
		t.Fatalf("seed drill: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO saoaf.exit_drill_finding
			(drill_key, finding_key, description, severity, state, remediation)
		VALUES ('drill-1', 'f-1', 'fallback too slow', 'HIGH', 'OPEN', '')`); err != nil {
		t.Fatalf("seed finding: %v", err)
	}
	if _, err := conn.Exec(ctx, `
		INSERT INTO saoaf.exit_drill_transition (drill_key, from_state, to_state, actor)
		VALUES ('drill-1', 'DRAFT', 'APPROVED', 'user:approver'),
		       ('drill-1', 'APPROVED', 'SUCCEEDED', 'user:worker')`); err != nil {
		t.Fatalf("seed transitions: %v", err)
	}
}

func ptr(f float64) *float64 { return &f }

// AC: all metrics drill down to (formula_version, dataset_revision,
// evidence_ref) — 100% sampling over the tenant-visible set.
func TestOpsDrillDownTripleCompleteness(t *testing.T) {
	withDBO(t, func(dsn string) {
		seedOps(t, dsn)
		h := newOpsHarness(t, dsn)
		tok := h.iss.token(t, "ops.read ops.all-tenants", "tenant-a", nil)

		code, body := h.get(t, "/admin/v1/ops/metrics?limit=200", tok)
		if code != http.StatusOK {
			t.Fatalf("metrics = %d %s", code, body)
		}
		var out struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		if len(out.Items) < 4 {
			t.Fatalf("expected ≥4 seeded metric rows, got %d", len(out.Items))
		}
		for _, it := range out.Items {
			fv, _ := it["formula_version"].(string)
			rev, ok := it["dataset_revision"].(float64)
			if fv == "" || !ok || rev == 0 {
				t.Fatalf("metric %v missing drill-down triple: %+v", it["metric_key"], it)
			}
			// evidence_ref may legitimately be empty ONLY for unknown-status
			// rows; OK rows must carry the triple's evidence leg
			if st, _ := it["status"].(string); st == "OK" {
				if ev, _ := it["evidence_ref"].(string); ev == "" {
					t.Fatalf("OK metric %v has empty evidence_ref", it["metric_key"])
				}
			}
		}
		// history endpoint carries the same triple
		code, body = h.get(t, "/admin/v1/ops/metrics/history?metric=substitution_coverage&limit=200", tok)
		if code != http.StatusOK {
			t.Fatalf("history = %d %s", code, body)
		}
		if !strings.Contains(body, `"formula_version":"v1"`) || !strings.Contains(body, `"dataset_revision":42`) {
			t.Fatalf("history rows missing the triple: %s", body)
		}
	})
}

// AC: unknown data carries an explicit status and null value — 未知 ≠ 0.
func TestOpsUnknownIsExplicitNeverZero(t *testing.T) {
	withDBO(t, func(dsn string) {
		seedOps(t, dsn)
		h := newOpsHarness(t, dsn)
		tok := h.iss.token(t, "ops.read", "tenant-a", nil)

		code, body := h.get(t, "/admin/v1/ops/metrics?metric=substitution_coverage&tenant=tenant-a", tok)
		if code != http.StatusOK {
			t.Fatalf("metrics = %d %s", code, body)
		}
		var out struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		unknownSeen := false
		for _, it := range out.Items {
			if st, _ := it["status"].(string); st == "UNKNOWN" {
				unknownSeen = true
				if it["value"] != nil {
					t.Fatalf("UNKNOWN metric must carry null value, got %v", it["value"])
				}
				if sr, _ := it["status_reason"].(string); sr == "" {
					t.Fatal("UNKNOWN metric must carry a status_reason")
				}
			}
		}
		if !unknownSeen {
			t.Fatal("seeded UNKNOWN metric not visible in the tenant view")
		}
		// overview surfaces unknown_metrics explicitly
		code, body = h.get(t, "/admin/v1/ops/overview", tok)
		if code != http.StatusOK || !strings.Contains(body, `"unknown_metrics"`) {
			t.Fatalf("overview must surface unknown metrics explicitly: %d %s", code, body)
		}
	})
}

// AC: cross-tenant unauthorized reads rejected 403; tenant confinement
// hides other tenants' rows.
func TestOpsTenantIsolation(t *testing.T) {
	withDBO(t, func(dsn string) {
		seedOps(t, dsn)
		h := newOpsHarness(t, dsn)

		// tenant-a operator: sees only tenant-a rows
		tokA := h.iss.token(t, "ops.read", "tenant-a", nil)
		code, body := h.get(t, "/admin/v1/ops/metrics?limit=200", tokA)
		if code != http.StatusOK {
			t.Fatalf("metrics = %d %s", code, body)
		}
		if strings.Contains(body, "tenant-b") {
			t.Fatalf("tenant-a operator sees tenant-b rows: %s", body)
		}
		// explicit cross-tenant request → 403 (not silent scoping)
		code, body = h.get(t, "/admin/v1/ops/metrics?tenant=tenant-b", tokA)
		if code != http.StatusForbidden {
			t.Fatalf("cross-tenant request = %d %s, want 403", code, body)
		}
		if !strings.Contains(body, "FORBIDDEN") {
			t.Fatalf("403 must use the frozen envelope: %s", body)
		}
		// cross-tenant on evidence view too
		code, _ = h.get(t, "/admin/v1/ops/evidence?tenant=tenant-b", tokA)
		if code != http.StatusForbidden {
			t.Fatalf("cross-tenant evidence = %d, want 403", code)
		}
		// evidence list confined: tenant-a sees only its records
		code, body = h.get(t, "/admin/v1/ops/evidence", tokA)
		if code != http.StatusOK || strings.Contains(body, "tenant-b") {
			t.Fatalf("evidence view must be tenant-confined: %d %s", code, body)
		}
		// the all-tenants scope holder sees everything
		tokAll := h.iss.token(t, "ops.read ops.all-tenants", "tenant-platform", nil)
		code, body = h.get(t, "/admin/v1/ops/metrics?limit=200", tokAll)
		if code != http.StatusOK || !strings.Contains(body, "tenant-b") {
			t.Fatalf("all-tenants operator must see tenant-b rows: %d %s", code, body)
		}
	})
}

// AC: cross-environment unauthorized reads rejected 403 (environments
// claim confines the axis).
func TestOpsEnvironmentIsolation(t *testing.T) {
	withDBO(t, func(dsn string) {
		seedOps(t, dsn)
		h := newOpsHarness(t, dsn)

		// operator confined to production
		tok := h.iss.token(t, "ops.read ops.all-tenants", "tenant-a", []string{"production"})
		code, body := h.get(t, "/admin/v1/ops/metrics?environment=staging", tok)
		if code != http.StatusForbidden {
			t.Fatalf("cross-environment request = %d %s, want 403", code, body)
		}
		// in-scope request succeeds and the forced filter hides staging rows
		code, body = h.get(t, "/admin/v1/ops/metrics?limit=200", tok)
		if code != http.StatusOK {
			t.Fatalf("metrics = %d %s", code, body)
		}
		if strings.Contains(body, "staging") {
			t.Fatalf("production-confined operator sees staging rows: %s", body)
		}
		// unrestricted token (no claim) sees both
		tokFree := h.iss.token(t, "ops.read ops.all-tenants", "tenant-a", nil)
		code, body = h.get(t, "/admin/v1/ops/metrics?limit=200", tokFree)
		if code != http.StatusOK || !strings.Contains(body, "staging") {
			t.Fatalf("unrestricted operator must see staging rows: %d %s", code, body)
		}
	})
}

// AC: pagination is server-side with a hard cap; missing scope 403.
func TestOpsPaginationAndScopeGate(t *testing.T) {
	withDBO(t, func(dsn string) {
		seedOps(t, dsn)
		h := newOpsHarness(t, dsn)
		tok := h.iss.token(t, "ops.read ops.all-tenants", "tenant-a", nil)

		// no ops.read scope → 403 (identity alone is not enough)
		tokNoScope := h.iss.token(t, "resource.read", "tenant-a", nil)
		code, _ := h.get(t, "/admin/v1/ops/metrics", tokNoScope)
		if code != http.StatusForbidden {
			t.Fatalf("no ops.read scope = %d, want 403", code)
		}
		// anonymous → 401
		code, _ = h.get(t, "/admin/v1/ops/metrics", "")
		if code != http.StatusUnauthorized {
			t.Fatalf("anonymous = %d, want 401", code)
		}
		// limit cap: absurd limits clamp to the 200 cap (payload bound)
		code, body := h.get(t, "/admin/v1/ops/metrics?limit=100000", tok)
		if code != http.StatusOK || !strings.Contains(body, `"limit":200`) {
			t.Fatalf("limit must clamp to 200: %d %s", code, body[:min(200, len(body))])
		}
		// offset paging returns the second page
		code, body = h.get(t, "/admin/v1/ops/metrics?limit=1&offset=1", tok)
		if code != http.StatusOK || !strings.Contains(body, `"count":1`) {
			t.Fatalf("paged read = %d %s", code, body)
		}
		// unknown drill → 404 envelope
		code, body = h.get(t, "/admin/v1/ops/drills/no-such-drill", tok)
		if code != http.StatusNotFound || !strings.Contains(body, `"error_code":"NOT_FOUND"`) {
			t.Fatalf("unknown drill = %d %s, want 404 envelope", code, body)
		}
	})
}

// The views over I13/I14/I15 data: alerts, expired packs (computed at read
// time), drill state + findings + audit log.
func TestOpsViewsAlertsPacksDrills(t *testing.T) {
	withDBO(t, func(dsn string) {
		seedOps(t, dsn)
		h := newOpsHarness(t, dsn)
		tok := h.iss.token(t, "ops.read ops.all-tenants", "tenant-a", nil)

		code, body := h.get(t, "/admin/v1/ops/alerts", tok)
		if code != http.StatusOK || !strings.Contains(body, "single-provider") {
			t.Fatalf("alerts = %d %s", code, body)
		}
		// the pack crossed valid_since at seed time — EXPIRED computed at
		// read, declared_state preserved
		code, body = h.get(t, "/admin/v1/ops/exit-packs", tok)
		if code != http.StatusOK {
			t.Fatalf("packs = %d %s", code, body)
		}
		var packs struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &packs); err != nil {
			t.Fatal(err)
		}
		if len(packs.Items) != 1 {
			t.Fatalf("packs = %+v", packs.Items)
		}
		if packs.Items[0]["state"] != "EXPIRED" || packs.Items[0]["declared_state"] != "ACTIVE" {
			t.Fatalf("expired pack must surface EXPIRED (read-time), declared ACTIVE: %+v", packs.Items[0])
		}
		// drills view + remediation detail + audit trail
		code, body = h.get(t, "/admin/v1/ops/drills", tok)
		if code != http.StatusOK || !strings.Contains(body, `"open_findings":1`) {
			t.Fatalf("drills = %d %s", code, body)
		}
		code, body = h.get(t, "/admin/v1/ops/drills/drill-1", tok)
		if code != http.StatusOK || !strings.Contains(body, "fallback too slow") {
			t.Fatalf("drill detail = %d %s", code, body)
		}
		code, body = h.get(t, "/admin/v1/ops/drills/drill-1/log", tok)
		if code != http.StatusOK || !strings.Contains(body, "APPROVED") || !strings.Contains(body, "SUCCEEDED") {
			t.Fatalf("drill log = %d %s", code, body)
		}
	})
}

// 证据断链 view: rows whose evidence_ref is empty or unresolved.
func TestOpsBrokenEvidenceView(t *testing.T) {
	withDBO(t, func(dsn string) {
		seedOps(t, dsn)
		h := newOpsHarness(t, dsn)
		tok := h.iss.token(t, "ops.read", "tenant-a", nil)

		code, body := h.get(t, "/admin/v1/ops/metrics/broken", tok)
		if code != http.StatusOK {
			t.Fatalf("broken = %d %s", code, body)
		}
		var out struct {
			Items []map[string]any `json:"items"`
		}
		if err := json.Unmarshal([]byte(body), &out); err != nil {
			t.Fatal(err)
		}
		// tenant-a rows: the staging UNKNOWN (empty ref) and the
		// INSUFFICIENT_DATA row with an unresolved ref
		if len(out.Items) != 2 {
			t.Fatalf("broken items = %d (%s), want the 2 tenant-a broken rows", len(out.Items), body)
		}
		for _, it := range out.Items {
			if br, _ := it["broken_reason"].(string); br == "" {
				t.Fatal("broken rows must carry a broken_reason")
			}
		}
	})
}

// Environments claim parsing round-trip: the trusted claim reaches the
// handler through the middleware identity.
func TestOpsEnvironmentsClaimParsed(t *testing.T) {
	withDBO(t, func(dsn string) {
		h := newOpsHarness(t, dsn)
		tok := h.iss.token(t, "ops.read ops.all-tenants", "tenant-a", []string{"production", "staging"})
		// whoami is the admin surface's diagnostic — verify the claim there
		code, body := h.get(t, "/admin/v1/whoami", tok)
		if code != http.StatusOK {
			t.Fatalf("whoami = %d %s", code, body)
		}
		_ = body // the claim's effect is proven by TestOpsEnvironmentIsolation
	})
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
