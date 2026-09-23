package workload

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"testing"
	"time"
)

type testCA struct {
	caKey  *rsa.PrivateKey
	caCert *x509.Certificate
}

func newTestCA(t *testing.T) *testCA {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "SAOAF test trust domain"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return &testCA{caKey: key, caCert: cert}
}

func (ca *testCA) leaf(t *testing.T, san *url.URL) *x509.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "resource-resolver"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		URIs:         []*url.URL{san},
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.caCert, &key.PublicKey, ca.caKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func mustSAN(t *testing.T, s string) *url.URL {
	t.Helper()
	u, err := url.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func pemCert(t *testing.T, cert *x509.Certificate) []byte {
	t.Helper()
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}

func TestVerifyHappyAndNegatives(t *testing.T) {
	ca := newTestCA(t)
	v, err := NewVerifier(pemCert(t, ca.caCert), "")
	if err != nil {
		t.Fatal(err)
	}

	san := mustSAN(t, "spiffe://saoaf.test/ns/default/sa/resource-resolver")
	if err := v.Verify(Chain{Leaf: ca.leaf(t, san)}); err != nil {
		t.Fatalf("valid SPIFFE workload cert rejected: %v", err)
	}

	// 未受信 trust domain → 拒绝
	other := mustSAN(t, "spiffe://evil.example/ns/default/sa/x")
	if err := v.Verify(Chain{Leaf: ca.leaf(t, other)}); err == nil {
		t.Fatal("untrusted SPIFFE domain accepted")
	}

	// 自签 rogue CA → 拒绝
	rogue := newTestCA(t)
	if err := v.Verify(Chain{Leaf: rogue.leaf(t, san)}); err == nil {
		t.Fatal("certificate from rogue CA accepted")
	}

	// 无证书 → 拒绝
	if err := v.Verify(Chain{}); err == nil {
		t.Fatal("empty chain accepted")
	}
}

func TestSANWorkloadID(t *testing.T) {
	ca := newTestCA(t)
	san := mustSAN(t, "spiffe://saoaf.test/ns/default/sa/resource-resolver")
	cert := ca.leaf(t, san)
	id, err := SANWorkloadID(cert)
	if err != nil || id != "default/sa/resource-resolver" {
		t.Fatalf("id=%q err=%v", id, err)
	}
}
