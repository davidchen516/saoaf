// Package authn validates OIDC access tokens for the control plane (I05).
//
// Phase 0 target: the Keycloak mock issuer
// http://127.0.0.1:8080/realms/saoaf-dev with audience saoaf-control-plane.
// Production swaps the issuer URL only; validation rules stay identical
// (issue #5 生产替换边界).
//
// Validation order is fixed: signature (JWKS) → issuer → audience → expiry.
// Any failure is a 401 — never a business error (issue routing priority).
package authn

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Identity is the trusted, server-side identity extracted from a verified
// token. Request-body claims never override these values (issue invariant:
// 服务端声明权威).
type Identity struct {
	Subject   string   // "sub"
	TenantRef string   // token claim "tenant_ref" (trusted; body values ignored)
	Scopes    []string // "scope" claim, space-split
	TokenID   string   // "jti" — replay/audit correlation
	Expiry    time.Time
	// Environments is the trusted "environments" claim (I17): the operator's
	// allowed environment scope. Absent/empty = unrestricted (platform
	// operator); otherwise sovereignty-ops views are confined to these
	// environments and explicit off-scope requests are rejected (403).
	Environments []string
}

// HasScope reports whether the identity carries a scope.
func (i *Identity) HasScope(s string) bool {
	for _, sc := range i.Scopes {
		if sc == s {
			return true
		}
	}
	return false
}

// Validator verifies access tokens against an OIDC issuer.
type Validator struct {
	issuer     string
	audience   string
	jwksURL    string
	httpClient *http.Client

	mu       sync.RWMutex
	keys     map[string]any // kid → parsed public key
	fetched  time.Time
	cacheTTL time.Duration
}

// NewValidator builds a Validator for issuer (e.g.
// http://127.0.0.1:8080/realms/saoaf-dev) and required audience.
func NewValidator(issuer, audience string) (*Validator, error) {
	if issuer == "" || audience == "" {
		return nil, errors.New("authn: issuer and audience are required")
	}
	wellKnown := strings.TrimRight(issuer, "/") + "/.well-known/openid-configuration"
	if _, err := url.Parse(wellKnown); err != nil {
		return nil, fmt.Errorf("authn: bad issuer: %w", err)
	}
	// Keycloak publishes JWKS at a standard path; fetch discovery once to be
	// exact instead of guessing.
	v := &Validator{
		issuer:     issuer,
		audience:   audience,
		jwksURL:    strings.TrimRight(issuer, "/") + "/protocol/openid-connect/certs",
		httpClient: &http.Client{Timeout: 5 * time.Second},
		keys:       map[string]any{},
		cacheTTL:   10 * time.Minute,
	}
	if jwks, err := v.discoverJWKS(context.Background()); err == nil {
		v.jwksURL = jwks
	}
	return v, nil
}

func (v *Validator) discoverJWKS(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(v.issuer, "/")+"/.well-known/openid-configuration", nil)
	if err != nil {
		return "", err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("discovery: status %d", resp.StatusCode)
	}
	var doc struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	if doc.JWKSURI == "" {
		return "", errors.New("discovery: missing jwks_uri")
	}
	return doc.JWKSURI, nil
}

type jwksDoc struct {
	Keys []struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// refreshKeys fetches the issuer JWKS when the cache is stale.
func (v *Validator) refreshKeys(ctx context.Context, force bool) error {
	v.mu.RLock()
	fresh := time.Since(v.fetched) < v.cacheTTL && len(v.keys) > 0
	v.mu.RUnlock()
	if fresh && !force {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("authn: jwks fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authn: jwks status %d", resp.StatusCode)
	}
	var doc jwksDoc
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return fmt.Errorf("authn: jwks decode: %w", err)
	}
	keys := map[string]any{}
	for _, k := range doc.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pk, err := jwkToRSAPublic(k.N, k.E)
		if err != nil {
			continue
		}
		keys[k.Kid] = pk
	}
	if len(keys) == 0 {
		return errors.New("authn: jwks has no usable keys")
	}
	v.mu.Lock()
	v.keys = keys
	v.fetched = time.Now()
	v.mu.Unlock()
	return nil
}

var ErrUnauthenticated = errors.New("unauthenticated")

// Validate verifies the raw token and returns the trusted Identity.
// Errors are deliberately coarse (ErrUnauthenticated with no token
// internals) so error responses cannot leak token details.
func (v *Validator) Validate(ctx context.Context, rawToken string) (*Identity, error) {
	// transport artifacts (trailing newline/whitespace) must not poison the
	// Authorization header downstream
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return nil, ErrUnauthenticated
	}
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithAudience(v.audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(5*time.Second),
	)
	var claims jwt.MapClaims
	_, err := parser.ParseWithClaims(rawToken, &claims, func(t *jwt.Token) (any, error) {
		kid, ok := t.Header["kid"].(string)
		if !ok || kid == "" {
			return nil, ErrUnauthenticated
		}
		if err := v.refreshKeys(ctx, false); err != nil {
			// key fetch failure → fail closed (do NOT accept unverified tokens)
			return nil, fmt.Errorf("%w: key source unavailable", ErrUnauthenticated)
		}
		v.mu.RLock()
		key := v.keys[kid]
		v.mu.RUnlock()
		if key == nil {
			// one forced refresh to cover key rotation, then fail closed
			if err := v.refreshKeys(ctx, true); err != nil {
				return nil, ErrUnauthenticated
			}
			v.mu.RLock()
			key = v.keys[kid]
			v.mu.RUnlock()
			if key == nil {
				return nil, ErrUnauthenticated
			}
		}
		return key, nil
	})
	if err != nil {
		return nil, ErrUnauthenticated
	}
	id := &Identity{TokenID: str(claims["jti"])}
	id.Subject = str(claims["sub"])
	if id.Subject == "" {
		// Keycloak 26 lightweight access tokens omit `sub` from the JWT.
		// The signature/issuer/audience/expiry checks above already passed;
		// resolve the subject from the issuer userinfo endpoint using the
		// same verified token. userinfo failure → fail closed (401).
		sub, err := v.userinfoSubject(ctx, rawToken)
		if err != nil {
			return nil, ErrUnauthenticated
		}
		id.Subject = sub
	}
	id.TenantRef = str(claims["tenant_ref"])
	if sc, ok := claims["scope"].(string); ok {
		id.Scopes = strings.Fields(sc)
	}
	if envs, ok := claims["environments"].([]any); ok {
		for _, e := range envs {
			if s := str(e); s != "" {
				id.Environments = append(id.Environments, s)
			}
		}
	}
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		id.Expiry = exp.Time
	}
	return id, nil
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// NewRequestID mints a UUIDv4-shaped correlation id when the caller
// supplies no X-Request-ID (downstream contracts validate UUID format).
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000-0000-0000-0000-000000000000"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// jwkToRSAPublic converts JWK base64url (n, e) to an *rsa.PublicKey.
func jwkToRSAPublic(nB64, eB64 string) (*rsa.PublicKey, error) {
	n, err := base64.RawURLEncoding.DecodeString(nB64)
	if err != nil {
		return nil, err
	}
	e, err := base64.RawURLEncoding.DecodeString(eB64)
	if err != nil || len(e) > 8 {
		return nil, fmt.Errorf("bad exponent")
	}
	exp := uint64(0)
	for _, b := range e {
		exp = exp<<8 | uint64(b)
	}
	pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(exp)}
	if pub.E == 0 || pub.E%2 == 0 {
		return nil, fmt.Errorf("bad exponent value")
	}
	return pub, nil
}

// userinfoSubject resolves the subject via the OIDC userinfo endpoint
// (Phase 0 Keycloak 26 lightweight-access-token enrichment). The token
// itself was already signature/issuer/audience/expiry verified.
func (v *Validator) userinfoSubject(ctx context.Context, rawToken string) (string, error) {
	url := strings.TrimRight(v.issuer, "/") + "/protocol/openid-connect/userinfo"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+rawToken)
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("userinfo status %d", resp.StatusCode)
	}
	var doc struct {
		Sub string `json:"sub"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", err
	}
	if doc.Sub == "" {
		return "", errors.New("userinfo missing sub")
	}
	return doc.Sub, nil
}
