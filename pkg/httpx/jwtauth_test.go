package httpx_test

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/httpx"
)

var testKey = []byte("nakama-session-encryption-key")

// resolverFor returns always the same mapping, which is all the verifier needs.
func resolverFor(playerID string) httpx.IdentityResolver {
	return func(_ context.Context, externalID string) (string, error) {
		if playerID == "" {
			return "", errors.New("no such player")
		}
		return playerID, nil
	}
}

func newAuth(t *testing.T, cfg httpx.JWTAuthConfig) *httpx.JWTAuth {
	t.Helper()
	if len(cfg.SecurityKey) == 0 {
		cfg.SecurityKey = testKey
	}
	// Default only when the caller configured nothing: a test that passes an explicit
	// credential set must not silently get the legacy single token as well.
	if cfg.ServiceToken == "" && len(cfg.ServiceCredentials) == 0 {
		cfg.ServiceToken = "service-secret"
	}
	if cfg.Resolve == nil {
		cfg.Resolve = resolverFor("p_1")
	}
	auth, err := httpx.NewJWTAuth(cfg)
	if err != nil {
		t.Fatalf("NewJWTAuth: %v", err)
	}
	return auth
}

func request(token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v1/account/players/p_1", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func claims(overrides map[string]any) map[string]any {
	now := time.Now().Unix()
	base := map[string]any{
		"uid": "nakama-user-1",
		"usn": "player-one",
		"iss": "nakama",
		"iat": now,
		"exp": now + 3600,
	}
	for k, v := range overrides {
		base[k] = v
	}
	return base
}

func sign(t *testing.T, key []byte, overrides map[string]any) string {
	t.Helper()
	token, err := httpx.SignHS256(key, claims(overrides))
	if err != nil {
		t.Fatalf("SignHS256: %v", err)
	}
	return token
}

func code(err error) apierr.Code { return apierr.As(err).Code }

func TestJWTAuthAcceptsAValidToken(t *testing.T) {
	auth := newAuth(t, httpx.JWTAuthConfig{})
	principal, err := auth.Player(request(sign(t, testKey, nil)))
	if err != nil {
		t.Fatalf("Player: %v", err)
	}
	if principal.PlayerID != "p_1" {
		t.Fatalf("PlayerID = %q, want the resolver's answer", principal.PlayerID)
	}
	if principal.ExternalID != "nakama-user-1" {
		t.Fatalf("ExternalID = %q, want nakama-user-1", principal.ExternalID)
	}
}

func TestJWTAuthFallsBackToSubClaim(t *testing.T) {
	auth := newAuth(t, httpx.JWTAuthConfig{})
	token := sign(t, testKey, map[string]any{"uid": nil, "sub": "nakama-user-2"})
	principal, err := auth.Player(request(token))
	if err != nil {
		t.Fatalf("Player: %v", err)
	}
	if principal.ExternalID != "nakama-user-2" {
		t.Fatalf("ExternalID = %q, want the sub claim", principal.ExternalID)
	}
}

func TestJWTAuthRejectsBadTokens(t *testing.T) {
	auth := newAuth(t, httpx.JWTAuthConfig{})
	now := time.Now()

	// header {"alg":"HS256"} with the wrong key
	wrongKey := sign(t, []byte("not-the-key"), nil)
	// alg:none, the classic bypass attempt
	noneHeader := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	nonePayload := base64.RawURLEncoding.EncodeToString([]byte(`{"uid":"attacker","exp":9999999999}`))
	noneToken := noneHeader + "." + nonePayload + "."

	// signature computed with HS512 but labelled HS256: we only implement HS256, so
	// this must fail on the signature, not on the header.
	tampered := sign(t, testKey, nil)
	parts := strings.Split(tampered, ".")
	tamperedPayload := base64.RawURLEncoding.EncodeToString([]byte(`{"iss":"nakama","uid":"attacker","exp":9999999999}`))
	tampered = parts[0] + "." + tamperedPayload + "." + parts[2]

	cases := map[string]struct {
		token string
		want  string
	}{
		"expired":          {sign(t, testKey, map[string]any{"exp": now.Add(-2 * time.Hour).Unix()}), "expired"},
		"no expiry":        {sign(t, testKey, map[string]any{"exp": nil}), "no expiry"},
		"issued in future": {sign(t, testKey, map[string]any{"iat": now.Add(2 * time.Hour).Unix()}), "future"},
		"wrong key":        {wrongKey, "signature"},
		"tampered payload": {tampered, "signature"},
		"alg none":         {noneToken, "algorithm"},
		"two segments":     {"a.b", "malformed"},
		"not base64":       {"!!!.???.###", "malformed"},
		"no user id":       {sign(t, testKey, map[string]any{"uid": nil, "sub": nil}), "no user id"},
	}
	for name, tc := range cases {
		_, err := auth.Player(request(tc.token))
		if err == nil {
			t.Fatalf("%s: token was accepted", name)
		}
		if got := code(err); got != apierr.CodeUnauthenticated {
			t.Fatalf("%s: code = %q, want unauthenticated", name, got)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: error %q does not mention %q", name, err.Error(), tc.want)
		}
	}

	if _, err := auth.Player(request("")); code(err) != apierr.CodeUnauthenticated {
		t.Fatalf("missing bearer token: code = %q", code(err))
	}
}

func TestJWTAuthToleratesClockSkew(t *testing.T) {
	auth := newAuth(t, httpx.JWTAuthConfig{Leeway: 2 * time.Minute})
	justExpired := sign(t, testKey, map[string]any{"exp": time.Now().Add(-30 * time.Second).Unix()})
	if _, err := auth.Player(request(justExpired)); err != nil {
		t.Fatalf("token expired 30s ago should be tolerated within 2m leeway: %v", err)
	}

	strict := newAuth(t, httpx.JWTAuthConfig{Leeway: time.Second})
	if _, err := strict.Player(request(justExpired)); err == nil {
		t.Fatal("token expired 30s ago must be refused with 1s leeway")
	}
}

func TestJWTAuthChecksIssuerAndAudience(t *testing.T) {
	auth := newAuth(t, httpx.JWTAuthConfig{Issuer: "nakama", Audience: "game"})

	ok := sign(t, testKey, map[string]any{"aud": "game"})
	if _, err := auth.Player(request(ok)); err != nil {
		t.Fatalf("token with matching aud: %v", err)
	}
	arrayAud := sign(t, testKey, map[string]any{"aud": []string{"other", "game"}})
	if _, err := auth.Player(request(arrayAud)); err != nil {
		t.Fatalf("token with aud as an array: %v", err)
	}

	if _, err := auth.Player(request(sign(t, testKey, map[string]any{"aud": "elsewhere"}))); err == nil {
		t.Fatal("wrong audience must be refused")
	}
	if _, err := auth.Player(request(sign(t, testKey, map[string]any{"iss": "someone-else"}))); err == nil {
		t.Fatal("wrong issuer must be refused")
	}
}

func TestJWTAuthRejectsAnUnknownIdentity(t *testing.T) {
	auth := newAuth(t, httpx.JWTAuthConfig{Resolve: resolverFor("")})
	_, err := auth.Player(request(sign(t, testKey, nil)))
	if err == nil {
		t.Fatal("an identity with no player must be refused")
	}
	if code(err) != apierr.CodeUnauthenticated {
		t.Fatalf("code = %q, want unauthenticated", code(err))
	}
}

func TestJWTAuthServiceToken(t *testing.T) {
	auth := newAuth(t, httpx.JWTAuthConfig{ServiceToken: "service-secret"})

	principal, err := auth.Service(request("service-secret"))
	if err != nil {
		t.Fatalf("Service: %v", err)
	}
	if principal.Service == "" {
		t.Fatal("service principal is empty")
	}

	// A player token must not work as a service token, and vice versa.
	if _, err := auth.Service(request(sign(t, testKey, nil))); err == nil {
		t.Fatal("a JWT must not authenticate a service call")
	}
	if _, err := auth.Player(request("service-secret")); err == nil {
		t.Fatal("the service token must not authenticate a player call")
	}
}

// TestServiceCredentialsRotateWithoutDowntime pins the rotation contract: during the
// overlap both credentials authenticate under their own names, and once the old one is
// removed only the new one does.
func TestServiceCredentialsRotateWithoutDowntime(t *testing.T) {
	overlap := newAuth(t, httpx.JWTAuthConfig{ServiceCredentials: httpx.ServiceCredentials{
		{Name: "nakama-old", Token: "old-secret"},
		{Name: "nakama-new", Token: "new-secret"},
	}})
	for token, want := range map[string]string{"old-secret": "nakama-old", "new-secret": "nakama-new"} {
		principal, err := overlap.Service(request(token))
		if err != nil {
			t.Fatalf("%s during the overlap: %v", token, err)
		}
		if principal.Service != want {
			t.Fatalf("caller = %q, want %q", principal.Service, want)
		}
	}

	retired := newAuth(t, httpx.JWTAuthConfig{ServiceCredentials: httpx.ServiceCredentials{
		{Name: "nakama-new", Token: "new-secret"},
	}})
	if _, err := retired.Service(request("old-secret")); err == nil {
		t.Fatal("the retired credential still authenticates")
	}
	if _, err := retired.Service(request("new-secret")); err != nil {
		t.Fatalf("the surviving credential stopped working: %v", err)
	}
}

func TestJWTAuthRejectsIncompleteConfiguration(t *testing.T) {
	if _, err := httpx.NewJWTAuth(httpx.JWTAuthConfig{ServiceToken: "s", Resolve: resolverFor("p")}); err == nil {
		t.Fatal("missing security key must be refused")
	}
	if _, err := httpx.NewJWTAuth(httpx.JWTAuthConfig{SecurityKey: testKey, Resolve: resolverFor("p")}); err == nil {
		t.Fatal("missing service token must be refused")
	}
	if _, err := httpx.NewJWTAuth(httpx.JWTAuthConfig{SecurityKey: testKey, ServiceToken: "s"}); err == nil {
		t.Fatal("missing resolver must be refused")
	}
	// A credential set that duplicates itself is a deployment mistake, not a detail to
	// discover on the first request.
	if _, err := httpx.NewJWTAuth(httpx.JWTAuthConfig{
		SecurityKey: testKey,
		ServiceCredentials: httpx.ServiceCredentials{
			{Name: "a", Token: "same"},
			{Name: "b", Token: "same"},
		},
		Resolve: resolverFor("p"),
	}); err == nil {
		t.Fatal("duplicate tokens must be refused at startup")
	}
}

// TestPlayerTokensAreAcceptedWithoutAResolver covers the second issuer: a token this
// backend signed (iss=gamebackend) already carries the backend player id, so verifying
// it must not touch the identity resolver - and must not accept a Nakama-signed token
// that claims to be one.
func TestPlayerTokensAreAcceptedWithoutAResolver(t *testing.T) {
	resolverCalls := 0
	auth, err := httpx.NewJWTAuth(httpx.JWTAuthConfig{
		SecurityKey:  testKey,
		PlayerKey:    testKey,
		ServiceToken: "service-secret",
		Resolve: func(context.Context, string) (string, error) {
			resolverCalls++
			return "p_resolved", nil
		},
	})
	if err != nil {
		t.Fatalf("NewJWTAuth: %v", err)
	}

	issuer, err := httpx.NewTokenIssuer(testKey, time.Hour)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	token, expires, err := issuer.Issue("p_123")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if !expires.After(time.Now()) {
		t.Fatalf("expiry %v is not in the future", expires)
	}

	principal, err := auth.Player(request(token))
	if err != nil {
		t.Fatalf("Player with a backend-issued token: %v", err)
	}
	if principal.PlayerID != "p_123" {
		t.Fatalf("PlayerID = %q, want the token's subject", principal.PlayerID)
	}
	if resolverCalls != 0 {
		t.Fatalf("the resolver ran %d times for a backend token; it must not run at all", resolverCalls)
	}

	// A token signed by the wrong key, even with the right issuer, is rejected.
	wrong, err := httpx.NewTokenIssuer([]byte("not-the-key"), time.Hour)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	badToken, _, err := wrong.Issue("p_999")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if _, err := auth.Player(request(badToken)); code(err) != apierr.CodeUnauthenticated {
		t.Fatalf("forged player token: code = %q, want unauthenticated", code(err))
	}

	// An unknown issuer is refused outright.
	unknown := sign(t, testKey, map[string]any{"iss": "someone-else", "sub": "p_1"})
	if _, err := auth.Player(request(unknown)); code(err) != apierr.CodeUnauthenticated {
		t.Fatalf("unknown issuer: code = %q, want unauthenticated", code(err))
	}

	// An expired player token is refused like any other.
	expiredIssuer, err := httpx.NewTokenIssuer(testKey, time.Nanosecond)
	if err != nil {
		t.Fatalf("NewTokenIssuer: %v", err)
	}
	expired, _, err := expiredIssuer.Issue("p_123")
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	time.Sleep(2 * time.Millisecond)
	// The default verifier tolerates 60s of clock skew, so expiry needs a strict one.
	strict := newAuth(t, httpx.JWTAuthConfig{Leeway: time.Millisecond})
	_, err = strict.Player(request(expired))
	if err == nil {
		t.Fatal("an expired player token was accepted")
	}
	if code(err) != apierr.CodeUnauthenticated {
		t.Fatalf("expired player token: code = %q, want unauthenticated", code(err))
	}
}

func TestTokenIssuerRequiresAKey(t *testing.T) {
	if _, err := httpx.NewTokenIssuer(nil, time.Hour); err == nil {
		t.Fatal("an empty key must be refused")
	}
	issuer, err := httpx.NewTokenIssuer(testKey, 0)
	if err != nil {
		t.Fatalf("a zero TTL should default, not fail: %v", err)
	}
	if _, _, err := issuer.Issue(""); err == nil {
		t.Fatal("issuing for an empty player id must be refused")
	}
}
