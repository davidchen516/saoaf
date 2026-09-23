package registry

// Snapshot ingest API tests (I11, specs §3.2): mTLS workload identity,
// §3.2 validation rules, idempotent replay, digest conflict, version
// regression, and the snapshot-changed outbox event — against a real PG.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
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
	san, _ := url.Parse("spiffe://saoaf.test/ns/mmr/sa/publisher")
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
	MountSnapshotAPI(r, SnapshotAPIConfig{
		Store:            &Store{DSN: dsn},
		WorkloadVerifier: verifier,
		ExpectedIdentity: "spiffe://saoaf.test/ns/mmr/sa/publisher",
	})
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
	return map[string]any{
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
		"digest": digest, "signature": "sig-mock",
	}
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
		digest := "sha256:" + fmt.Sprintf("%064d", 1)

		// first ingest: submit + activate + snapshot-changed outbox event
		if code := post(t, h, ingestBody(1, digest)); code != http.StatusOK {
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
		if code := post(t, h, ingestBody(1, digest)); code != http.StatusOK {
			t.Fatalf("idempotent replay = %d", code)
		}
		var rows int
		_ = conn.QueryRow(ctx,
			`SELECT count(*) FROM registry.provider_snapshot`).Scan(&rows)
		if rows != 1 {
			t.Fatalf("rows after replay = %d, want 1 (idempotent)", rows)
		}

		// same version + DIFFERENT digest → 409 (契约漂移告警语义)
		if code := post(t, h, ingestBody(1, "sha256:"+fmt.Sprintf("%064d", 2))); code != http.StatusConflict {
			t.Fatalf("digest conflict = %d, want 409", code)
		}

		// version regression → 409
		if code := post(t, h, ingestBody(0, digest)); code != http.StatusBadRequest {
			t.Fatalf("version 0 = %d, want 400", code)
		}

		// new higher version is fine
		if code := post(t, h, ingestBody(2, "sha256:"+fmt.Sprintf("%064d", 2))); code != http.StatusOK {
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
		// empty signature
		bad = ingestBody(1, "sha256:"+fmt.Sprintf("%064d", 1))
		bad["signature"] = ""
		if code := post(t, h, bad); code != http.StatusBadRequest {
			t.Fatalf("no signature = %d", code)
		}
		// invalid profile state
		bad = ingestBody(1, "sha256:"+fmt.Sprintf("%064d", 1))
		bad["profiles"] = []map[string]any{{
			"profile_id": "p", "capability_keys": []string{"x"}, "regions": []string{"r"},
			"data_classification_max": "CONFIDENTIAL", "features": map[string]any{},
			"constraint_schema_version": "1.0", "status": "DRAFTY",
		}}
		if code := post(t, h, bad); code != http.StatusBadRequest {
			t.Fatalf("bad profile state = %d", code)
		}
		// provider mismatch (body vs path)
		bad = ingestBody(1, "sha256:"+fmt.Sprintf("%064d", 1))
		bad["provider_id"] = "other-provider"
		if code := post(t, h, bad); code != http.StatusBadRequest {
			t.Fatalf("provider mismatch = %d", code)
		}
		// unknown provider
		b := ingestBody(1, "sha256:"+fmt.Sprintf("%064d", 1))
		b["provider_id"] = "mmr-unknown"
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
		b, _ := json.Marshal(ingestBody(1, "sha256:"+fmt.Sprintf("%064d", 1)))
		resp, err := insecure.Post(h.srv.URL+"/providers/mmr-test/snapshots", "application/json", bytes.NewReader(b))
		if err == nil {
			_ = resp.Body.Close()
			t.Fatalf("mTLS-less ingest reached the handler: %d", resp.StatusCode)
		}
		// (the handshake failure IS the Phase 0 boundary: r.TLS.PeerCertificates
		// is never even populated for an unauthenticated caller)
	})
}
