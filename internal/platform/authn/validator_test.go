package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// testIssuer is an OIDC + JWKS test double with its own RSA key.
type testIssuer struct {
	key   *rsa.PrivateKey
	kid   string
	jwks  *httptest.Server
	disco *httptest.Server
}

func newTestIssuer(t *testing.T) *testIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &testIssuer{key: key, kid: "test-kid-1"}

	iss.jwks = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"keys": []any{map[string]string{
				"kty": "RSA", "kid": iss.kid, "alg": "RS256", "use": "sig",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}},
		})
	}))

	iss.disco = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/openid-configuration" {
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":   iss.disco.URL,
				"jwks_uri": iss.jwks.URL,
			})
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(iss.jwks.Close)
	t.Cleanup(iss.disco.Close)
	return iss
}

// issue mints a signed token with the given claims.
func (iss *testIssuer) issue(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = iss.kid
	s, err := tok.SignedString(iss.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func validClaims(iss *testIssuer) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":        iss.disco.URL,
		"aud":        "saoaf-control-plane",
		"sub":        "user:alice",
		"jti":        "tok-0001",
		"scope":      "resource.read resource.publish",
		"tenant_ref": "tenant-demo",
		"exp":        time.Now().Add(5 * time.Minute).Unix(),
	}
}

func newValidator(t *testing.T, iss *testIssuer) *Validator {
	t.Helper()
	v, err := NewValidator(iss.disco.URL, "saoaf-control-plane")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestValidateHappyPath(t *testing.T) {
	iss := newTestIssuer(t)
	v := newValidator(t, iss)

	id, err := v.Validate(context.Background(), iss.issue(t, validClaims(iss)))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if id.Subject != "user:alice" || id.TenantRef != "tenant-demo" || !id.HasScope("resource.publish") {
		t.Fatalf("identity = %+v", id)
	}
	if !id.HasScope("resource.read") || id.HasScope("drill.approve") {
		t.Fatal("scope parsing wrong")
	}
}

// GWT#2 负向套件：过期/伪造签名/错 issuer/错 audience/无 kid/坏 JWT。
func TestValidateRejects(t *testing.T) {
	iss := newTestIssuer(t)
	other := newTestIssuer(t) // 不同密钥 → 伪造签名
	v := newValidator(t, iss)

	mint := func(c jwt.MapClaims) string { return iss.issue(t, c) }
	forbidden := newTestIssuerForbidden{mint, other, iss, v}
	forbidden.run(t)
}

type newTestIssuerForbidden struct {
	mint  func(jwt.MapClaims) string
	other *testIssuer
	iss   *testIssuer
	v     *Validator
}

func (f newTestIssuerForbidden) run(t *testing.T) {
	t.Helper()
	cases := []struct {
		name  string
		token func() string
	}{
		{"expired token", func() string {
			c := validClaims(f.iss)
			c["exp"] = time.Now().Add(-time.Hour).Unix()
			return f.mint(c)
		}},
		{"forged signature (other key)", func() string {
			c := validClaims(f.iss)
			tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
			tok.Header["kid"] = f.iss.kid
			s, err := tok.SignedString(f.other.key)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}},
		{"wrong issuer", func() string {
			c := validClaims(f.iss)
			c["iss"] = "http://evil.example"
			return f.mint(c)
		}},
		{"wrong audience", func() string {
			c := validClaims(f.iss)
			c["aud"] = "some-other-api"
			return f.mint(c)
		}},
		{"missing subject", func() string {
			c := validClaims(f.iss)
			delete(c, "sub")
			return f.mint(c)
		}},
		{"alg=none", func() string {
			tok := jwt.NewWithClaims(jwt.SigningMethodNone, validClaims(f.iss))
			tok.Header["kid"] = f.iss.kid
			s, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
			if err != nil {
				t.Fatal(err)
			}
			return s
		}},
		{"garbage", func() string { return "not-a-jwt" }},
		{"empty", func() string { return "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.v.Validate(context.Background(), tc.token()); err == nil {
				t.Fatal("token unexpectedly accepted")
			}
		})
	}
}

// JWKS 不可用 → fail closed（不接受未验证 token）。
func TestValidateFailsClosedWhenJWKSDown(t *testing.T) {
	iss := newTestIssuer(t)
	v := newValidator(t, iss)

	// 先用有效 JWKS 验证一次（初始化 key 缓存之外的 discovery 路径）
	if _, err := v.Validate(context.Background(), iss.issue(t, validClaims(iss))); err != nil {
		t.Fatalf("setup validate: %v", err)
	}
	iss.jwks.Close()
	// 强制刷新（新 kid）→ key 获取失败 → fail closed
	c := validClaims(iss)
	c["jti"] = "tok-0002"
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	forged.Header["kid"] = "rotated-key"
	s, err := forged.SignedString(iss.key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := v.Validate(context.Background(), s); err == nil {
		t.Fatal("token with unknown kid accepted while JWKS down — fail-open violation")
	}
}

// key rotation: JWKS 换新 kid 后，新 token 有效。
func TestValidateFollowsKeyRotation(t *testing.T) {
	iss := newTestIssuer(t)
	v := newValidator(t, iss)
	if _, err := v.Validate(context.Background(), iss.issue(t, validClaims(iss))); err != nil {
		t.Fatalf("initial: %v", err)
	}
	iss.kid = "rotated-kid-2"
	c := validClaims(iss)
	c["jti"] = "tok-0003"
	if _, err := v.Validate(context.Background(), iss.issue(t, c)); err != nil {
		t.Fatalf("rotated key rejected: %v", err)
	}
}
