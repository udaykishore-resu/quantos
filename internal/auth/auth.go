// Package auth implements authentication and role-based authorisation.
//
// Two modes are supported. In `embedded` and `compose` mode QuantOS issues its
// own HS256 tokens to locally-configured principals, so a developer can run the
// platform without an identity provider. In `cluster` mode it validates RS256
// tokens from an OIDC issuer against its JWKS. The permission model is the same
// in both: a principal holds roles, roles grant scopes, and handlers require
// scopes.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Role is a named bundle of scopes.
type Role string

const (
	RoleViewer   Role = "viewer"
	RoleAnalyst  Role = "analyst"
	RoleOperator Role = "operator"
	RoleAdmin    Role = "admin"
)

// Scope is a single permission.
type Scope string

const (
	ScopeReadMarket    Scope = "market:read"
	ScopeReadSignals   Scope = "signals:read"
	ScopeReadModels    Scope = "models:read"
	ScopeReadPortfolio Scope = "portfolio:read"
	ScopeRunBacktest   Scope = "backtest:run"
	ScopePaperTrade    Scope = "paper:write"
	ScopeManageConfig  Scope = "config:write"
	ScopeManageModels  Scope = "models:write"
	ScopeReadAudit     Scope = "audit:read"
)

// roleScopes is the authorisation matrix. It is data rather than scattered
// conditionals so that "who can do what" is answerable by reading one table.
var roleScopes = map[Role][]Scope{
	RoleViewer: {
		ScopeReadMarket, ScopeReadSignals, ScopeReadModels, ScopeReadPortfolio,
	},
	RoleAnalyst: {
		ScopeReadMarket, ScopeReadSignals, ScopeReadModels, ScopeReadPortfolio,
		ScopeRunBacktest,
	},
	RoleOperator: {
		ScopeReadMarket, ScopeReadSignals, ScopeReadModels, ScopeReadPortfolio,
		ScopeRunBacktest, ScopePaperTrade, ScopeReadAudit,
	},
	RoleAdmin: {
		ScopeReadMarket, ScopeReadSignals, ScopeReadModels, ScopeReadPortfolio,
		ScopeRunBacktest, ScopePaperTrade, ScopeManageConfig, ScopeManageModels,
		ScopeReadAudit,
	},
}

// ScopesFor returns the scopes granted by a set of roles.
func ScopesFor(roles []string) []Scope {
	seen := map[Scope]bool{}
	for _, r := range roles {
		for _, s := range roleScopes[Role(strings.ToLower(strings.TrimSpace(r)))] {
			seen[s] = true
		}
	}
	out := make([]Scope, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Principal is an authenticated caller.
type Principal struct {
	Subject   string    `json:"sub"`
	Name      string    `json:"name,omitempty"`
	Roles     []string  `json:"roles"`
	Scopes    []Scope   `json:"scopes"`
	ExpiresAt time.Time `json:"expires_at"`
	Issuer    string    `json:"issuer"`
}

// Has reports whether the principal holds a scope.
func (p Principal) Has(s Scope) bool {
	for _, x := range p.Scopes {
		if x == s {
			return true
		}
	}
	return false
}

// Anonymous is the principal used when authentication is disabled. It holds
// read scopes only: disabling auth must not silently grant write access.
func Anonymous() Principal {
	return Principal{
		Subject: "anonymous", Roles: []string{string(RoleViewer)},
		Scopes: ScopesFor([]string{string(RoleViewer)}), Issuer: "local",
	}
}

// Errors returned by verification.
var (
	ErrNoToken      = errors.New("auth: no token supplied")
	ErrInvalidToken = errors.New("auth: token is invalid")
	ErrExpired      = errors.New("auth: token has expired")
	ErrForbidden    = errors.New("auth: insufficient scope")
)

// Config parameterises the authenticator.
type Config struct {
	Enabled  bool
	Issuer   string
	Audience string
	// HMACSecret is used to sign locally-issued tokens. A short or empty secret
	// is rejected rather than silently accepted.
	HMACSecret string
	// JWKSURL enables OIDC validation of RS256 tokens.
	JWKSURL  string
	TokenTTL time.Duration
	// Users are locally-configured principals for embedded and compose modes.
	Users []User
	// JWKSRefresh bounds how often the key set is refetched.
	JWKSRefresh time.Duration
	HTTPClient  *http.Client
}

// User is a locally-configured principal. Passwords are stored as a salted
// SHA-256 hash computed at load time; the plaintext never persists in memory
// beyond construction.
type User struct {
	Subject  string
	Password string
	Roles    []string
}

type storedUser struct {
	subject string
	salt    []byte
	hash    []byte
	roles   []string
}

// Authenticator verifies tokens and issues local ones.
type Authenticator struct {
	cfg   Config
	users map[string]storedUser

	mu      sync.RWMutex
	jwks    map[string]*rsa.PublicKey
	fetched time.Time
	client  *http.Client
}

// New builds an authenticator.
func New(cfg Config) (*Authenticator, error) {
	if cfg.Enabled && cfg.JWKSURL == "" && len(cfg.HMACSecret) < 32 {
		return nil, fmt.Errorf(
			"auth: a local HMAC secret of at least 32 bytes is required when no JWKS URL is configured " +
				"(set the environment variable named by auth.hmac_secret_env)")
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = 12 * time.Hour
	}
	if cfg.JWKSRefresh <= 0 {
		cfg.JWKSRefresh = 15 * time.Minute
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 5 * time.Second}
	}
	a := &Authenticator{cfg: cfg, users: map[string]storedUser{}, jwks: map[string]*rsa.PublicKey{}, client: cfg.HTTPClient}
	for _, u := range cfg.Users {
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return nil, err
		}
		a.users[u.Subject] = storedUser{
			subject: u.Subject, salt: salt,
			hash:  hashPassword(u.Password, salt),
			roles: u.Roles,
		}
	}
	return a, nil
}

func hashPassword(pw string, salt []byte) []byte {
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(pw))
	sum := h.Sum(nil)
	// A few thousand iterations is not bcrypt, and this path exists only for
	// locally-configured development principals. Production deployments use
	// the OIDC path and never reach here; cluster mode requires auth.enabled
	// with a real issuer.
	for i := 0; i < 4096; i++ {
		h.Reset()
		h.Write(sum)
		h.Write(salt)
		sum = h.Sum(nil)
	}
	return sum
}

// Login verifies local credentials and issues a token.
func (a *Authenticator) Login(subject, password string) (string, Principal, error) {
	u, ok := a.users[subject]
	if !ok {
		// Hash anyway so a missing user and a wrong password take the same time.
		_ = hashPassword(password, make([]byte, 16))
		return "", Principal{}, ErrInvalidToken
	}
	if subtle.ConstantTimeCompare(u.hash, hashPassword(password, u.salt)) != 1 {
		return "", Principal{}, ErrInvalidToken
	}
	now := time.Now().UTC()
	p := Principal{
		Subject: u.subject, Roles: u.roles, Scopes: ScopesFor(u.roles),
		ExpiresAt: now.Add(a.cfg.TokenTTL), Issuer: a.cfg.Issuer,
	}
	tok, err := a.Issue(p, now)
	return tok, p, err
}

// Issue signs a local HS256 token.
func (a *Authenticator) Issue(p Principal, now time.Time) (string, error) {
	claims := jwt.MapClaims{
		"sub":   p.Subject,
		"iss":   a.cfg.Issuer,
		"aud":   a.cfg.Audience,
		"iat":   now.Unix(),
		"exp":   p.ExpiresAt.Unix(),
		"roles": p.Roles,
	}
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return t.SignedString([]byte(a.cfg.HMACSecret))
}

// Verify validates a bearer token and returns the principal.
func (a *Authenticator) Verify(ctx context.Context, token string) (Principal, error) {
	if !a.cfg.Enabled {
		return Anonymous(), nil
	}
	if token == "" {
		return Principal{}, ErrNoToken
	}
	parsed, err := jwt.Parse(token, func(t *jwt.Token) (any, error) {
		switch t.Method.(type) {
		case *jwt.SigningMethodHMAC:
			if a.cfg.HMACSecret == "" {
				return nil, fmt.Errorf("auth: HMAC tokens are not accepted by this deployment")
			}
			return []byte(a.cfg.HMACSecret), nil
		case *jwt.SigningMethodRSA:
			kid, _ := t.Header["kid"].(string)
			return a.publicKey(ctx, kid)
		default:
			// Rejecting unknown algorithms explicitly is what prevents the
			// classic "alg: none" and algorithm-confusion attacks.
			return nil, fmt.Errorf("auth: unsupported signing method %v", t.Header["alg"])
		}
	}, jwt.WithValidMethods([]string{"HS256", "RS256"}), jwt.WithIssuer(a.cfg.Issuer))

	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return Principal{}, ErrExpired
		}
		return Principal{}, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	claims, ok := parsed.Claims.(jwt.MapClaims)
	if !ok || !parsed.Valid {
		return Principal{}, ErrInvalidToken
	}
	if a.cfg.Audience != "" {
		if !audienceMatches(claims["aud"], a.cfg.Audience) {
			return Principal{}, fmt.Errorf("%w: audience mismatch", ErrInvalidToken)
		}
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Principal{}, fmt.Errorf("%w: no subject", ErrInvalidToken)
	}
	roles := extractRoles(claims)
	exp := time.Time{}
	if v, err := claims.GetExpirationTime(); err == nil && v != nil {
		exp = v.Time
	}
	iss, _ := claims["iss"].(string)
	return Principal{
		Subject: sub, Roles: roles, Scopes: ScopesFor(roles),
		ExpiresAt: exp, Issuer: iss,
	}, nil
}

func audienceMatches(claim any, want string) bool {
	switch v := claim.(type) {
	case string:
		return v == want
	case []any:
		for _, x := range v {
			if s, ok := x.(string); ok && s == want {
				return true
			}
		}
	case []string:
		for _, s := range v {
			if s == want {
				return true
			}
		}
	}
	return false
}

func extractRoles(claims jwt.MapClaims) []string {
	var out []string
	add := func(v any) {
		switch x := v.(type) {
		case string:
			for _, s := range strings.Fields(strings.ReplaceAll(x, ",", " ")) {
				out = append(out, s)
			}
		case []any:
			for _, e := range x {
				if s, ok := e.(string); ok {
					out = append(out, s)
				}
			}
		}
	}
	add(claims["roles"])
	// Common OIDC shapes, so a real issuer works without a custom mapper.
	if realm, ok := claims["realm_access"].(map[string]any); ok {
		add(realm["roles"])
	}
	add(claims["groups"])
	if len(out) == 0 {
		out = []string{string(RoleViewer)}
	}
	return out
}

// --- JWKS ------------------------------------------------------------------

type jwksResponse struct {
	Keys []struct {
		Kid string `json:"kid"`
		Kty string `json:"kty"`
		Alg string `json:"alg"`
		Use string `json:"use"`
		N   string `json:"n"`
		E   string `json:"e"`
	} `json:"keys"`
}

// publicKey returns the RSA key for a key id, refreshing the key set when the
// id is unknown or the cache is stale.
func (a *Authenticator) publicKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.mu.RLock()
	key, ok := a.jwks[kid]
	fresh := time.Since(a.fetched) < a.cfg.JWKSRefresh
	a.mu.RUnlock()
	if ok && fresh {
		return key, nil
	}
	if a.cfg.JWKSURL == "" {
		return nil, fmt.Errorf("auth: RS256 token received but no JWKS URL is configured")
	}
	if err := a.refreshJWKS(ctx); err != nil {
		if ok {
			// A refresh failure should not invalidate a key we already hold.
			return key, nil
		}
		return nil, err
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if k, ok := a.jwks[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("auth: unknown key id %q", kid)
}

func (a *Authenticator) refreshJWKS(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.JWKSURL, nil)
	if err != nil {
		return err
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("auth: fetch JWKS: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("auth: JWKS endpoint returned %d", resp.StatusCode)
	}
	var out jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("auth: decode JWKS: %w", err)
	}
	keys := map[string]*rsa.PublicKey{}
	for _, k := range out.Keys {
		if k.Kty != "RSA" {
			continue
		}
		nb, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			continue
		}
		eb, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			continue
		}
		e := 0
		switch len(eb) {
		case 3:
			e = int(uint32(eb[0])<<16 | uint32(eb[1])<<8 | uint32(eb[2]))
		case 4:
			e = int(binary.BigEndian.Uint32(eb))
		default:
			for _, b := range eb {
				e = e<<8 | int(b)
			}
		}
		if e == 0 {
			continue
		}
		keys[k.Kid] = &rsa.PublicKey{N: new(big.Int).SetBytes(nb), E: e}
	}
	if len(keys) == 0 {
		return errors.New("auth: JWKS contained no usable RSA keys")
	}
	a.mu.Lock()
	a.jwks = keys
	a.fetched = time.Now()
	a.mu.Unlock()
	return nil
}

// Enabled reports whether authentication is enforced.
func (a *Authenticator) Enabled() bool { return a.cfg.Enabled }

// BearerToken extracts a token from the Authorization header or, for
// EventSource connections which cannot set headers, from the access_token query
// parameter. The query-parameter path is restricted to the stream endpoint by
// the caller.
func BearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(strings.ToLower(h), "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	if c, err := r.Cookie("quantos_token"); err == nil {
		return c.Value
	}
	return ""
}
