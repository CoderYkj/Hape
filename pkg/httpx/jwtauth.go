package httpx

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"gamebackend/pkg/apierr"
)

// JWTAuth authenticates players with a Nakama-issued session token and services
// with a shared secret.
//
// Nakama signs session tokens with HS256 using the server's session encryption key;
// the payload carries the Nakama user id (`uid`, older builds `sub`), plus `exp`,
// `iat` and `iss`. This verifier is deliberately strict: HS256 only, signature
// compared in constant time, `exp` required. "Strict" here means "an attacker
// cannot choose the algorithm or skip the expiry".
//
// The verifier does not know what a backend player id is. A token carries the
// Nakama identity; mapping that to this backend's player record is the resolver's
// job (see IdentityResolver), so pkg/httpx stays free of domain knowledge.
type JWTAuth struct {
	key          []byte
	serviceToken string
	issuer       string
	audience     string
	leeway       time.Duration
	now          func() time.Time
	resolve      IdentityResolver
}

// IdentityResolver maps an external identity (a Nakama user id) to a backend
// player id. It must be read-only: authentication must not create accounts.
type IdentityResolver func(ctx context.Context, externalID string) (playerID string, err error)

// JWTAuthConfig is the verifier's configuration.
type JWTAuthConfig struct {
	// SecurityKey is Nakama's session encryption key (BACKEND_JWT_KEY).
	SecurityKey []byte
	// ServiceToken authenticates server-to-server callers (BACKEND_SERVICE_TOKEN).
	ServiceToken string
	// Issuer and Audience are optional; empty disables the check.
	Issuer   string
	Audience string
	// Leeway tolerates clock skew between Nakama and this process.
	Leeway time.Duration
	// Now is injectable for tests.
	Now func() time.Time
	// Resolve maps a verified Nakama user id to a backend player id.
	Resolve IdentityResolver
}

// NewJWTAuth validates the configuration and returns the authenticator.
//
// A verifier without a key or without a resolver is a deployment mistake that must
// fail at startup, not reject every request at runtime.
func NewJWTAuth(cfg JWTAuthConfig) (*JWTAuth, error) {
	if len(cfg.SecurityKey) == 0 {
		return nil, errors.New("httpx: JWT security key is required")
	}
	if cfg.ServiceToken == "" {
		return nil, errors.New("httpx: service token is required next to JWT auth")
	}
	if cfg.Resolve == nil {
		return nil, errors.New("httpx: identity resolver is required")
	}
	if cfg.Leeway <= 0 {
		cfg.Leeway = 60 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &JWTAuth{
		key:          cfg.SecurityKey,
		serviceToken: cfg.ServiceToken,
		issuer:       cfg.Issuer,
		audience:     cfg.Audience,
		leeway:       cfg.Leeway,
		now:          cfg.Now,
		resolve:      cfg.Resolve,
	}, nil
}

// Player implements Authenticator.
func (a *JWTAuth) Player(r *http.Request) (Principal, error) {
	token := bearer(r)
	if token == "" {
		return Principal{}, apierr.Unauthenticated("missing bearer token")
	}

	externalID, err := a.verify(token)
	if err != nil {
		return Principal{}, err
	}

	playerID, err := a.resolve(r.Context(), externalID)
	if err != nil {
		// The token is valid but this backend has no player for it yet: the account
		// has not been created, which is a 401 rather than a 500.
		return Principal{}, apierr.Unauthenticated("no player for this identity").WithCause(err)
	}
	return Principal{PlayerID: playerID, ExternalID: externalID}, nil
}

// Service implements Authenticator. Service callers keep using the shared token:
// they are trusted processes, not users, and a stolen JWT must not be enough to
// submit a battle report.
func (a *JWTAuth) Service(r *http.Request) (Principal, error) {
	presented := bearer(r)
	if presented == "" {
		return Principal{}, apierr.Unauthenticated("missing service token")
	}
	if !hmac.Equal([]byte(presented), []byte(a.serviceToken)) {
		return Principal{}, apierr.Unauthenticated("invalid service token")
	}
	return Principal{Service: "service"}, nil
}

// verify checks the signature, the algorithm and the time claims, and returns the
// external identity.
func (a *JWTAuth) verify(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", apierr.Unauthenticated("malformed token")
	}

	// The algorithm is taken from the header, so it must be pinned. Accepting
	// "none" or letting the token choose is the classic JWT bypass.
	var header struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := decodeSegment(parts[0], &header); err != nil {
		return "", apierr.Unauthenticated("malformed token header")
	}
	if header.Alg != "HS256" {
		return "", apierr.Unauthenticated("unsupported token algorithm").WithDetail("alg", header.Alg)
	}

	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", apierr.Unauthenticated("malformed token signature")
	}
	mac := hmac.New(sha256.New, a.key)
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return "", apierr.Unauthenticated("bad token signature")
	}

	var claims jwtClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return "", apierr.Unauthenticated("malformed token claims")
	}

	now := a.now()
	if claims.Exp == nil {
		return "", apierr.Unauthenticated("token has no expiry")
	}
	if now.After(time.Unix(*claims.Exp, 0).Add(a.leeway)) {
		return "", apierr.Unauthenticated("token expired")
	}
	if claims.Iat != nil && time.Unix(*claims.Iat, 0).After(now.Add(a.leeway)) {
		return "", apierr.Unauthenticated("token issued in the future")
	}
	if a.issuer != "" && claims.Iss != a.issuer {
		return "", apierr.Unauthenticated("unexpected token issuer").WithDetail("iss", claims.Iss)
	}
	if a.audience != "" && !claims.hasAudience(a.audience) {
		return "", apierr.Unauthenticated("unexpected token audience")
	}

	externalID := claims.externalID()
	if externalID == "" {
		return "", apierr.Unauthenticated("token has no user id")
	}
	return externalID, nil
}

// jwtClaims is the subset of a Nakama session token this backend reads.
type jwtClaims struct {
	Sub string          `json:"sub"`
	UID string          `json:"uid"`
	USN string          `json:"usn"`
	Iss string          `json:"iss"`
	Aud json.RawMessage `json:"aud"`
	Exp *int64          `json:"exp"`
	Iat *int64          `json:"iat"`
}

// externalID prefers Nakama's `uid` and falls back to the standard `sub`.
func (c jwtClaims) externalID() string {
	if c.UID != "" {
		return c.UID
	}
	return c.Sub
}

// hasAudience accepts both the string and the array form of `aud`.
func (c jwtClaims) hasAudience(want string) bool {
	if len(c.Aud) == 0 {
		return false
	}
	var single string
	if err := json.Unmarshal(c.Aud, &single); err == nil {
		return single == want
	}
	var many []string
	if err := json.Unmarshal(c.Aud, &many); err == nil {
		for _, aud := range many {
			if aud == want {
				return true
			}
		}
	}
	return false
}

func decodeSegment[T any](segment string, out *T) error {
	raw, err := base64.RawURLEncoding.DecodeString(segment)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// SignHS256 builds a token with the same shape Nakama issues. It exists so tests
// (and local tools) can mint tokens without a Nakama instance.
func SignHS256(key []byte, claims map[string]any) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("httpx: marshal claims: %w", err)
	}
	body := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(body))
	return body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
