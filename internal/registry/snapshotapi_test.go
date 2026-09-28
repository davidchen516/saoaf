package registry

// Snapshot ingest API tests (I11, specs §3.2): mTLS workload identity,
// §3.2 validation rules, idempotent replay, digest conflict, version
// regression, and the snapshot-changed outbox event — against a real PG.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"

	"github.com/davidchen516/saoaf/internal/platform/workload"
)

// connectHelper reuses the store's connection helper in tests.
func connectHelper(ctx context.Context, dsn string) (*pgx.Conn, error) {
	s := Store{DSN: dsn}
	return s.connect(ctx)
}

func withDBReg(t *testing.T, fn func(dsn string)) {
	t.Helper()
	base := os.Getenv("SAOAF_TEST_PG_DSN")
	if base == "" {
		t.Skip("SAOAF_TEST_PG_DSN not set")
	}
	ctx := context.Background()
	admin, err := connectHelper(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(ctx) })
	name := fmt.Sprintf("registry_ingest_%d_%d", os.Getpid(), time.Now().UnixNano())
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

// seedProviderReg seeds a PUBLISHED provider for ingest tests.
func seedProviderReg(t *testing.T, dsn, key string) {
	t.Helper()
	ctx := context.Background()
	conn, err := connectHelper(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `
		INSERT INTO registry.resource_provider
			(provider_key, provider_type, endpoint_ref, owner_ref, workload_identity, state, revision, active_revision)
		VALUES ($1, 'MODEL', 'svc://test/mmr', 'user:t', 'spiffe://saoaf.test/ns/mmr/sa/publisher', 'PUBLISHED', 1, 1)`,
		key); err != nil {
		t.Fatal(err)
	}
}

// mTLS harness: CA + publisher leaf + mutually-authenticated test server.
type mtlsHarness struct {
	srv     *httptest.Server
	client  *http.Client
	leafPEM tls.Certificate
}

func newMTLSHarness(t *testing.T, dsn string) *mtlsHarness {
	return newMTLSHarnessCfg(t, dsn, "")
}

func newMTLSHarnessWithSAN(t *testing.T, dsn, san string) *mtlsHarness {
	return newMTLSHarnessFull(t, dsn, "", san)
}

func newMTLSHarnessCfg(t *testing.T, dsn, contractMajor string) *mtlsHarness {
	return newMTLSHarnessFull(t, dsn, contractMajor, "spiffe://saoaf.test/ns/mmr/sa/publisher")
}

// newMTLSHarnessPublisherKey wires a publisher Ed25519 key (ledger #2
// signature verification path).
func newMTLSHarnessPublisherKey(t *testing.T, dsn string, pub ed25519.PublicKey) *mtlsHarness {
	t.Helper()
	return newMTLSHarnessFull(t, dsn, "", "spiffe://saoaf.test/ns/mmr/sa/publisher", pub)
}

func newMTLSHarnessFull(t *testing.T, dsn, contractMajor, publisherSAN string, publisherKey ...ed25519.PublicKey) *mtlsHarness {
	t.Helper()
	// CA
	caKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "saoaf test CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert, _ := x509.ParseCertificate(caDER)
	// publisher leaf with the expected SPIFFE SAN
	leafKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	san, _ := url.Parse(publisherSAN)
	leafTpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "mmr-publisher"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		URIs: []*url.URL{san}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage: x509.KeyUsageDigitalSignature,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTpl, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	leafKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(leafKey)})
	clientCert, err := tls.X509KeyPair(leafPEM, leafKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	// verifier over the CA
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	verifier, err := workload.NewVerifier(caPEM, "spiffe://saoaf.test/")
	if err != nil {
		t.Fatal(err)
	}

	r := chi.NewRouter()
	apiCfg := SnapshotAPIConfig{
		Store:                 &Store{DSN: dsn},
		WorkloadVerifier:      verifier,
		ExpectedIdentity:      "spiffe://saoaf.test/ns/mmr/sa/publisher",
		ExpectedContractMajor: contractMajor,
	}
	if len(publisherKey) > 0 {
		apiCfg.PublisherKey = publisherKey[0]
	}
	MountSnapshotAPI(r, apiCfg)
	_ = publisherSAN // the CLIENT cert carries this SAN; the server accepts only the publisher one
	// server leaf signed by the same CA (the client trusts only this CA)
	serverKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	serverTpl := &x509.Certificate{
		SerialNumber: big.NewInt(3), Subject: pkix.Name{CommonName: "ingest test server"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{"127.0.0.1"},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTpl, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	serverPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})
	serverCert, err := tls.X509KeyPair(serverPEM, serverKeyPEM)
	if err != nil {
		t.Fatal(err)
	}

	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	srv := httptest.NewUnstartedServer(r)
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS12,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{clientCert},
			RootCAs:      caPool,
		},
	}}
	return &mtlsHarness{srv: srv, client: client, leafPEM: clientCert}
}

func ingestBody(version int, digest string) map[string]any {
	body := map[string]any{
		"provider_id": "mmr-test", "snapshot_version": version,
		"contract_version": "2026.09",
		"generated_at":     time.Now().UTC().Format(time.RFC3339),
		"valid_until":      time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		"profiles": []map[string]any{{
			"profile_id": "reasoning-high-v1", "capability_keys": []string{"model.reasoning.high"},
			"regions": []string{"cn-east"}, "data_classification_max": "CONFIDENTIAL",
			"features": map[string]any{"streaming": false}, "constraint_schema_version": "1.0",
			"status": "AVAILABLE",
		}},
		"digest": "", "signature": "sig-mock",
	}
	if digest == "" {
		// content-addressed by construction: round-trip through the request
		// struct and digest the canonical content (ledger #2 — publishers
		// that follow the documented canonicalization always match)
		b, _ := json.Marshal(body)
		var req IngestRequest
		if err := json.Unmarshal(b, &req); err != nil {
			panic("test ingest body round-trip: " + err.Error())
		}
		body["digest"] = SnapshotDigest(&req)
	} else {
		body["digest"] = digest
	}
	return body
}

// digestFor recomputes the content-addressed digest for a body map (the
// test-side publisher contract, same as ingestBody's built-in path).
func digestFor(t *testing.T, body map[string]any) string {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	var req IngestRequest
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	return SnapshotDigest(&req)
}

func post(t *testing.T, h *mtlsHarness, body map[string]any) int {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := h.client.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var rb bytes.Buffer
	_, _ = rb.ReadFrom(resp.Body)
	if resp.StatusCode >= 300 {
		t.Logf("ingest response %d: %s", resp.StatusCode, rb.String())
	}
	return resp.StatusCode
}

func TestSnapshotIngestFlow(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarness(t, dsn)

		// first ingest: submit + activate + snapshot-changed outbox event
		if code := post(t, h, ingestBody(1, "")); code != http.StatusOK {
			t.Fatalf("first ingest = %d", code)
		}
		ctx := context.Background()
		conn, err := connectHelper(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var events int
		if err := conn.QueryRow(ctx, `
			SELECT count(*) FROM saoaf.outbox_event
			WHERE topic = 'provider.snapshot-changed' AND aggregate_id = 'mmr-test'`).Scan(&events); err != nil {
			t.Fatal(err)
		}
		if events != 1 {
			t.Fatalf("snapshot-changed outbox events = %d, want 1 (same-tx)", events)
		}

		// idempotent replay (same version + digest) → 200 no-op
		if code := post(t, h, ingestBody(1, "")); code != http.StatusOK {
			t.Fatalf("idempotent replay = %d", code)
		}
		var rows int
		_ = conn.QueryRow(ctx,
			`SELECT count(*) FROM registry.provider_snapshot`).Scan(&rows)
		if rows != 1 {
			t.Fatalf("rows after replay = %d, want 1 (idempotent)", rows)
		}

		// same version + different content → recomputed digest differs from
		// the committed row → 409 契约漂移告警 (content addressing bounds
		// the digest — it can never be a stale/foreign string)
		drift := ingestBody(1, "")
		drift["valid_until"] = time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339)
		// recompute over the MUTATED content: the digest is honest for what
		// is sent — the conflict comes from the committed row (same version,
		// different content digest), not from a stale string
		if raw, merr := json.Marshal(drift); merr == nil {
			var r IngestRequest
			if json.Unmarshal(raw, &r) == nil {
				drift["digest"] = SnapshotDigest(&r)
			}
		}
		if code := post(t, h, drift); code != http.StatusConflict {
			t.Fatalf("digest conflict = %d, want 409", code)
		}
		// version 2 is a NEW version with its own content digest → 200
		if code := post(t, h, ingestBody(2, "")); code != http.StatusOK {
			t.Fatalf("new version = %d, want 200", code)
		}

		// version regression → 409
		if code := post(t, h, ingestBody(0, "")); code != http.StatusBadRequest {
			t.Fatalf("version 0 = %d, want 400", code)
		}

		// new higher version is fine
		if code := post(t, h, ingestBody(2, "")); code != http.StatusOK {
			t.Fatalf("version 2 ingest = %d", code)
		}
	})
}

func TestSnapshotIngestValidation(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarness(t, dsn)

		// bad digest
		bad := ingestBody(1, "md5:xxx")
		if code := post(t, h, bad); code != http.StatusBadRequest {
			t.Fatalf("bad digest = %d", code)
		}
		// empty signature (content-matching digest: the SIGNATURE branch fires)
		bad = ingestBody(1, "")
		bad["signature"] = ""
		if code := post(t, h, bad); code != http.StatusBadRequest {
			t.Fatalf("no signature = %d", code)
		}
		// invalid profile state (content-matching digest recomputed over the
		// bad profiles — the INVALID_PROFILE_STATE branch fires)
		bad = ingestBody(1, "")
		bad["profiles"] = []map[string]any{{
			"profile_id": "p", "capability_keys": []string{"x"}, "regions": []string{"r"},
			"data_classification_max": "CONFIDENTIAL", "features": map[string]any{},
			"constraint_schema_version": "1.0", "status": "DRAFTY",
		}}
		if code := post(t, h, bad); code != http.StatusBadRequest {
			t.Fatalf("bad profile state = %d", code)
		}
		// provider mismatch (body vs path)
		bad = ingestBody(1, "sha256:"+fmt.Sprintf("%064d", 9))
		bad["provider_id"] = "other-provider"
		if code := post(t, h, bad); code != http.StatusBadRequest {
			t.Fatalf("provider mismatch = %d", code)
		}
		// unknown provider
		b := ingestBody(1, "")
		b["provider_id"] = "mmr-unknown"
		// recompute the digest over the CHANGED content so the rejection
		// exercises the unknown-provider branch, not DIGEST_MISMATCH
		if raw, merr := json.Marshal(b); merr == nil {
			var r IngestRequest
			if json.Unmarshal(raw, &r) == nil {
				b["digest"] = SnapshotDigest(&r)
			}
		}
		jb, _ := json.Marshal(b)
		resp, err := h.client.Post(h.srv.URL+"/providers/mmr-unknown/snapshots", "application/json", bytes.NewReader(jb))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("unknown provider = %d, want 404", resp.StatusCode)
		}
	})
}

func TestSnapshotIngestWorkloadIdentity(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarness(t, dsn)

		// no client certificate (plain https to the mTLS server fails at
		// the TLS handshake) — assert the endpoint requires mTLS at all
		insecure := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true, // no client cert
		}}}
		b, _ := json.Marshal(ingestBody(1, ""))
		resp, err := insecure.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("mTLS-less ingest reached the handler: %d", resp.StatusCode)
		}
		// (the handshake failure IS the Phase 0 boundary: r.TLS.PeerCertificates
		// is never even populated for an unauthenticated caller)
	})
}

// R1-P1 回归（审查探针 E 正名）：半提交（submit 落行、未激活）后的重放
// 不得谎报 idempotent——必须恢复激活（Resumable → 重试 activate）。
func TestSnapshotIngestHalfCommitReplayResumes(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarness(t, dsn)
		// content-addressed: the half-committed DRAFT row must carry the
		// SAME digest the replay will send, so compute it from the ingest
		// body content for version 5
		replayBody := ingestBody(5, "")
		digest := digestFor(t, replayBody)

		// half commit: the row lands via the store path but activation
		// never runs (simulating a crash between submit and activate)
		store := Store{DSN: dsn}
		conn, _ := connectHelper(context.Background(), dsn)
		var pid int64
		_ = conn.QueryRow(context.Background(),
			`SELECT id FROM registry.resource_provider WHERE provider_key='mmr-test'`).Scan(&pid)
		now := time.Now()
		snap := &Snapshot{ProviderID: pid, SnapshotVersion: 5,
			ContractVersion: "2026.09", Digest: digest, Signature: "sig-mock",
			WorkloadIdentity: "spiffe://saoaf.test/ns/mmr/sa/publisher",
			GeneratedAt:      now.Add(-time.Minute), ValidUntil: now.Add(time.Hour)}
		if err := store.SubmitSnapshot(context.Background(), snap, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		var state string
		_ = conn.QueryRow(context.Background(),
			`SELECT state FROM registry.provider_snapshot WHERE snapshot_version=5`).Scan(&state)
		if state != "DRAFT" {
			t.Fatalf("half-commit state = %s, want DRAFT", state)
		}

		// replay the same (version, digest): NOT idempotent — activation
		// resumes and completes
		if code := post(t, h, replayBody); code != http.StatusOK {
			t.Fatalf("resumption ingest = %d (want activation retry, not fake idempotent)", code)
		}
		_ = conn.QueryRow(context.Background(),
			`SELECT state FROM registry.provider_snapshot WHERE snapshot_version=5`).Scan(&state)
		if state != "PUBLISHED" {
			t.Fatalf("state after replay = %s, want PUBLISHED (half-commit converged)", state)
		}
		var pointers int
		_ = conn.QueryRow(context.Background(),
			`SELECT count(*) FROM registry.provider_active_pointer WHERE provider_id=$1`, pid).Scan(&pointers)
		if pointers != 1 {
			t.Fatalf("active pointers = %d, want 1", pointers)
		}

		// and an expired-window variant: replay stays honestly rejected —
		// never a fake idempotent success
		expired := &Snapshot{ProviderID: pid, SnapshotVersion: 6,
			ContractVersion: "2026.09", Digest: "sha256:" + fmt.Sprintf("%064d", 8), Signature: "sig-mock",
			WorkloadIdentity: "spiffe://saoaf.test/ns/mmr/sa/publisher",
			GeneratedAt:      now.Add(-2 * time.Hour), ValidUntil: now.Add(-time.Hour)}
		if err := store.SubmitSnapshot(context.Background(), expired, map[string]any{}); err != nil {
			t.Fatal(err)
		}
		// the replay carries the SAME expired window as the seeded row —
		// the P1 bug reported "idempotent 200" here; the fix must reject
		// honestly (window check) instead of faking success
		body := ingestBody(6, "")
		body["generated_at"] = now.Add(-2 * time.Hour).UTC().Format(time.RFC3339)
		body["valid_until"] = now.Add(-time.Hour).UTC().Format(time.RFC3339)
		b, _ := json.Marshal(body)
		resp, err := h.client.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expired-window replay = %d, want honest 400 (no fake idempotent)", resp.StatusCode)
		}
		var stateAfter string
		_ = conn.QueryRow(context.Background(),
			`SELECT state FROM registry.provider_snapshot WHERE snapshot_version=6`).Scan(&stateAfter)
		if stateAfter != "DRAFT" {
			t.Fatalf("expired replay changed state to %s", stateAfter)
		}
	})
}

// R1 P2-2 回归：并发同 (version,digest) 双请求——loser 经重分类得到幂等 200
// （而非裸唯一冲突 400）；双请求收敛为恰一次 activate。
func TestSnapshotIngestConcurrentSameKeyConverges(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarness(t, dsn)
		body := ingestBody(3, "") // content-addressed

		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				b, _ := json.Marshal(body)
				resp, err := h.client.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
				if err != nil {
					t.Errorf("concurrent ingest: %v", err)
					return
				}
				defer resp.Body.Close()
				codes[i] = resp.StatusCode
			}(i)
		}
		wg.Wait()
		for _, c := range codes {
			if c != http.StatusOK {
				t.Fatalf("concurrent same-key codes = %v, want both 200 (activate + idempotent/reclassified)", codes)
			}
		}
		conn, _ := connectHelper(context.Background(), dsn)
		var snaps, pointers int
		_ = conn.QueryRow(context.Background(),
			`SELECT count(*) FROM registry.provider_snapshot WHERE snapshot_version=3`).Scan(&snaps)
		_ = conn.QueryRow(context.Background(),
			`SELECT count(*) FROM registry.provider_active_pointer`).Scan(&pointers)
		if snaps != 1 || pointers != 1 {
			t.Fatalf("snaps=%d pointers=%d, want 1/1 (恰一次 activate)", snaps, pointers)
		}
	})
}

// 真版本回退：无行版本 < 已存在最大版本 → 409 VERSION_REGRESSION。
func TestSnapshotIngestVersionRegression409(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarness(t, dsn)
		if code := post(t, h, ingestBody(1, "")); code != http.StatusOK {
			t.Fatalf("v1 = %d", code)
		}
		if code := post(t, h, ingestBody(3, "")); code != http.StatusOK {
			t.Fatalf("v3 = %d", code)
		}
		// v2 has no row but is below the current max (3)
		b, _ := json.Marshal(ingestBody(2, ""))
		resp, err := h.client.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusConflict {
			t.Fatalf("regression ingest = %d, want 409", resp.StatusCode)
		}
		var env struct {
			ErrorCode string `json:"error_code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		if env.ErrorCode != "SNAPSHOT_VERSION_REGRESSION" {
			t.Fatalf("code = %s", env.ErrorCode)
		}
	})
}

// 契约 major 为配置期望（P3-3）：错配 → 400 CONTRACT_MAJOR_REJECTED。
func TestSnapshotIngestContractMajorConfig(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarnessCfg(t, dsn, "2026")
		// matching major passes
		body := ingestBody(1, "")
		if code := post(t, h, body); code != http.StatusOK {
			t.Fatalf("matching major = %d", code)
		}
		// mismatch rejected BEFORE any write — no garbage DRAFT row
		// (R2 review probe M adopted; P3-3 check moved ahead of submit)
		bad := ingestBody(2, "")
		bad["contract_version"] = "1999.01"
		// recompute over the mutated content so the rejection exercises
		// the CONTRACT_MAJOR branch, not DIGEST_MISMATCH
		bad["digest"] = digestFor(t, bad)
		b, _ := json.Marshal(bad)
		resp, err := h.client.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("mismatched major = %d, want 400", resp.StatusCode)
		}
		var env struct {
			ErrorCode string `json:"error_code"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&env)
		if env.ErrorCode != "CONTRACT_MAJOR_REJECTED" {
			t.Fatalf("code = %s", env.ErrorCode)
		}
		conn, _ := connectHelper(context.Background(), dsn)
		var rows int
		_ = conn.QueryRow(context.Background(),
			`SELECT count(*) FROM registry.provider_snapshot WHERE snapshot_version = 2`).Scan(&rows)
		if rows != 0 {
			t.Fatalf("mismatched major left %d garbage rows (check must precede submit)", rows)
		}
	})
}

// 异 SAN 身份 → 403 IDENTITY_MISMATCH（审查探针 G 正名）。
func TestSnapshotIngestWrongSAN403(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		// harness with a client cert carrying a DIFFERENT SPIFFE SAN
		h := newMTLSHarnessWithSAN(t, dsn, "spiffe://saoaf.test/ns/other/sa/publisher")
		b, _ := json.Marshal(ingestBody(1, ""))
		resp, err := h.client.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatalf("wrong-SAN ingest = %d, want 403", resp.StatusCode)
		}
	})
}

// I11 mock-closure regressions (David 2026-09-28 directive).

// TestSnapshotIngestDigestRecompute (ledger #2): a syntactically valid digest
// that does NOT bind the content is rejected — blue-team: dropping the
// recompute check lets a stale digest ride.
func TestSnapshotIngestDigestRecompute(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarness(t, dsn)
		// foreign digest (valid format, wrong content) → DIGEST_MISMATCH
		if code := post(t, h, ingestBody(1, "sha256:"+fmt.Sprintf("%064d", 42))); code != http.StatusBadRequest {
			t.Fatalf("foreign digest = %d, want 400 DIGEST_MISMATCH", code)
		}
		// matching digest → 200
		if code := post(t, h, ingestBody(1, "")); code != http.StatusOK {
			t.Fatalf("content digest = %d, want 200", code)
		}
	})
}

// TestSnapshotIngestSignatureVerify (ledger #2): with a configured publisher
// key, only a valid ed25519 signature over the digest passes — an unsigned
// presence-only string AND a properly-formed signature from the WRONG key
// are both rejected (the wrong-key case is what makes the ed25519.Verify
// call itself falsifiable).
func TestSnapshotIngestSignatureVerify(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		pub, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		h := newMTLSHarnessPublisherKey(t, dsn, pub)
		// unsigned (presence-only string) → SIGNATURE_INVALID
		body := ingestBody(1, "")
		if code := post(t, h, body); code != http.StatusBadRequest {
			t.Fatalf("unverified signature = %d, want 400", code)
		}
		// wrong key: valid base64, 64 bytes, signed by a different keypair —
		// only the ed25519.Verify call can catch this one
		_, wrongPriv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		wrong := ingestBody(1, "")
		wrong["signature"] = base64.StdEncoding.EncodeToString(
			ed25519.Sign(wrongPriv, []byte(wrong["digest"].(string))))
		if code := post(t, h, wrong); code != http.StatusBadRequest {
			t.Fatalf("wrong-key signature = %d, want 400 SIGNATURE_INVALID", code)
		}
		// properly signed over the digest → 200
		sig := ed25519.Sign(priv, []byte(body["digest"].(string)))
		body["signature"] = base64.StdEncoding.EncodeToString(sig)
		if code := post(t, h, body); code != http.StatusOK {
			t.Fatalf("signed ingest = %d, want 200", code)
		}
	})
}

// TestSnapshotVersionStringForm (ledger #3): snapshot_version arrives as a
// JSON string on the wire (the contract/mock form "1") — accepted, stored as
// int; non-numeric strings are a typed rejection.
func TestSnapshotVersionStringForm(t *testing.T) {
	withDBReg(t, func(dsn string) {
		seedProviderReg(t, dsn, "mmr-test")
		h := newMTLSHarness(t, dsn)
		// string-form numeric version, POSTed as raw bytes (no re-marshal,
		// which would revert the edit): the digest binds the DECODED content,
		// identical for both forms, so ingestBody's digest stays valid
		body := ingestBody(1, "")
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		b = bytes.Replace(b, []byte(`"snapshot_version":1`), []byte(`"snapshot_version":"1"`), 1)
		resp, err := h.client.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("string-form version = %d, want 200 (type unification)", resp.StatusCode)
		}
		// stored as int: the string form lands as integer 1 in the DB chain
		ctx := context.Background()
		conn, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close(ctx)
		var stored int
		if err := conn.QueryRow(ctx,
			`SELECT snapshot_version FROM registry.provider_snapshot WHERE provider_id =
			 (SELECT id FROM registry.resource_provider WHERE provider_key='mmr-test')`).Scan(&stored); err != nil {
			t.Fatal(err)
		}
		if stored != 1 {
			t.Fatalf("stored snapshot_version = %d (type %T), want int 1", stored, stored)
		}
		// non-numeric string → typed rejection (mutate the MAP, not the
		// marshaled bytes — a second Marshal would revert the edit)
		bad := ingestBody(2, "")
		bad["snapshot_version"] = "mock-9"
		bb, _ := json.Marshal(bad)
		resp2, err := h.client.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(bb))
		if err != nil {
			t.Fatal(err)
		}
		resp2.Body.Close()
		if resp2.StatusCode != http.StatusBadRequest {
			t.Fatalf("non-numeric version = %d, want 400", resp2.StatusCode)
		}
	})
}
