package platform_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gamebackend/pkg/httpx"
	"gamebackend/platform"
)

// TestJWTModeIsUsableEndToEnd is the acceptance test for the production
// authenticator: with dev auth off, a Nakama-signed token authenticates a player
// whose account was created through the public login endpoint.
func TestJWTModeIsUsableEndToEnd(t *testing.T) {
	const key = "nakama-session-encryption-key"

	app, err := platform.New(platform.Config{
		Env:          "staging",
		DevAuth:      false,
		ServiceToken: "service-secret",
		JWTKey:       key,
		Allocator:    "local",
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("platform.New in JWT mode: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	// Login is a service-only exchange (ADR 0010): anonymous callers cannot mint a
	// token for an arbitrary identity any more.
	status, _ := doJSON(t, server.URL, http.MethodPost, "/v1/account/sessions",
		map[string]any{"external_id": "nakama-user-9"}, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("anonymous login = %d, want 401", status)
	}

	status, body := doJSON(t, server.URL, http.MethodPost, "/v1/account/sessions",
		map[string]any{"external_id": "nakama-user-9"}, "service-secret")
	if status != http.StatusOK {
		t.Fatalf("login status = %d (%v)", status, body)
	}
	playerID, _ := body["player"].(map[string]any)["id"].(string)
	if playerID == "" {
		t.Fatalf("login returned no player id: %v", body)
	}

	token := func(uid string, keyBytes []byte, ttl time.Duration) string {
		signed, err := httpx.SignHS256(keyBytes, map[string]any{
			"uid": uid,
			"exp": time.Now().Add(ttl).Unix(),
			"iat": time.Now().Unix(),
		})
		if err != nil {
			t.Fatalf("sign: %v", err)
		}
		return signed
	}

	// The token login handed back is a signed JWT this backend accepts (iss=gamebackend),
	// so the client gets a credential that works outside dev too.
	sessionToken, _ := body["session_token"].(string)
	if sessionToken == "" {
		t.Fatalf("login returned no session token: %v", body)
	}
	if !strings.HasPrefix(sessionToken, "eyJ") {
		t.Fatalf("production login returned %q, want a JWT", sessionToken)
	}
	status, body = doJSON(t, server.URL, http.MethodGet, "/v1/account/players/"+playerID, nil, sessionToken)
	if status != http.StatusOK {
		t.Fatalf("authenticated read = %d (%v)", status, body)
	}

	// A token signed with the wrong key is refused.
	status, _ = doJSON(t, server.URL, http.MethodGet, "/v1/account/players/"+playerID, nil,
		token("nakama-user-9", []byte("wrong-key"), time.Hour))
	if status != http.StatusUnauthorized {
		t.Fatalf("forged token = %d, want 401", status)
	}

	// An expired token is refused.
	status, _ = doJSON(t, server.URL, http.MethodGet, "/v1/account/players/"+playerID, nil,
		token("nakama-user-9", []byte(key), -2*time.Hour))
	if status != http.StatusUnauthorized {
		t.Fatalf("expired token = %d, want 401", status)
	}

	// A verified identity with no account behind it is refused, not created.
	status, _ = doJSON(t, server.URL, http.MethodGet, "/v1/account/players/"+playerID, nil,
		token("nakama-user-without-account", []byte(key), time.Hour))
	if status != http.StatusUnauthorized {
		t.Fatalf("unknown identity = %d, want 401", status)
	}

	// The development bearer format must not work once dev auth is off.
	status, _ = doJSON(t, server.URL, http.MethodGet, "/v1/account/players/"+playerID, nil, "")
	if status != http.StatusUnauthorized {
		t.Fatalf("no token in JWT mode = %d, want 401", status)
	}
	req, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/account/players/"+playerID, nil)
	req.Header.Set("Authorization", "Bearer dev-player-"+playerID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("dev token request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("dev bearer token in JWT mode = %d, want 401", resp.StatusCode)
	}
}

// doJSON posts/gets JSON and returns the status and decoded body.
func doJSON(t *testing.T, base, method, path string, body any, bearer string) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// TestDevAuthAcceptsARotatingCredentialSet exercises the same mechanism in the mode the
// local drills use, so rotation is not only covered by the production code path.
func TestDevAuthAcceptsARotatingCredentialSet(t *testing.T) {
	app, err := platform.New(platform.Config{
		Env:       "dev",
		DevAuth:   true,
		Allocator: "local",
		ServiceCredentials: httpx.ServiceCredentials{
			// Wildcard scopes: this test is about rotation, and an empty scope now means
			// "may call nothing" (fail closed), so it would fail for the wrong reason.
			{Name: "dev-old", Token: "dev-old-secret", Services: []string{httpx.AllServices}},
			{Name: "dev-new", Token: "dev-new-secret", Services: []string{httpx.AllServices}},
		},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("platform.New: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	for _, token := range []string{"dev-old-secret", "dev-new-secret"} {
		status, body := doJSON(t, server.URL, http.MethodPost, "/v1/account/sessions",
			map[string]any{"external_id": "rotating-" + token}, token)
		if status != http.StatusOK {
			t.Fatalf("%s was rejected: %d (%v)", token, status, body)
		}
	}

	// Anything that is not in the set is refused, including the default dev token the
	// process did not configure.
	status, _ := doJSON(t, server.URL, http.MethodPost, "/v1/account/sessions",
		map[string]any{"external_id": "rotating-stranger"}, "dev-service")
	if status != http.StatusUnauthorized {
		t.Fatalf("unconfigured token = %d, want 401", status)
	}
}

// TestServiceScopeIsEnforcedPerService is the least-privilege contract end to end: a
// credential scoped to `account` can exchange identities and nothing else, and the refusal
// is 403 rather than 401 - the credential is real, the action is not allowed, and those two
// mean different things to whoever is on call.
func TestServiceScopeIsEnforcedPerService(t *testing.T) {
	scoped := httpx.ServiceCredentials{
		{Name: "nakama", Token: "nakama-secret", Services: []string{"account"}},
	}
	newServer := func(t *testing.T, observe bool) string {
		t.Helper()
		app, err := platform.New(platform.Config{
			Env:                  "dev",
			DevAuth:              true,
			Allocator:            "local",
			ServiceCredentials:   scoped,
			ObserveServiceScopes: observe,
			Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatalf("platform.New: %v", err)
		}
		t.Cleanup(func() { _ = app.Close() })
		server := httptest.NewServer(app.Handler())
		t.Cleanup(server.Close)
		return server.URL
	}

	enforced := newServer(t, false)
	status, _ := doJSON(t, enforced, http.MethodPost, "/v1/account/sessions",
		map[string]any{"external_id": "scoped-player"}, "nakama-secret")
	if status != http.StatusOK {
		t.Fatalf("in-scope call = %d, want 200", status)
	}

	status, body := doJSON(t, enforced, http.MethodPost, "/v1/session/sessions",
		map[string]any{"match_id": "m_scope", "players": []string{"p_1", "p_2"}}, "nakama-secret")
	if status != http.StatusForbidden {
		t.Fatalf("out-of-scope call = %d, want 403 (%v)", status, body)
	}
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "permission_denied" {
		t.Fatalf("error code = %q, want permission_denied", code)
	}

	// Observe mode is the rollout path: the same call succeeds and is logged as
	// "would deny", which is how a policy gets validated without breaking anything.
	observed := newServer(t, true)
	status, _ = doJSON(t, observed, http.MethodPost, "/v1/session/sessions",
		map[string]any{"match_id": "m_observe", "players": []string{"p_1", "p_2"}}, "nakama-secret")
	if status != http.StatusOK {
		t.Fatalf("observe mode = %d, want 200 (nothing should be refused yet)", status)
	}
}

// TestScopeRefusalsAreCounted is how the observe-then-enforce rollout gets a number: in
// observe mode out-of-scope calls increment the would-deny counter (and the deployment
// keeps working), and once enforcement is on the same call increments the denied counter.
func TestScopeRefusalsAreCounted(t *testing.T) {
	scoped := httpx.ServiceCredentials{
		{Name: "nakama", Token: "nakama-secret", Services: []string{"account"}},
	}
	newServer := func(t *testing.T, observe bool) string {
		t.Helper()
		app, err := platform.New(platform.Config{
			Env:                  "dev",
			DevAuth:              true,
			Allocator:            "local",
			ServiceCredentials:   scoped,
			ObserveServiceScopes: observe,
			Logger:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatalf("platform.New: %v", err)
		}
		t.Cleanup(func() { _ = app.Close() })
		server := httptest.NewServer(app.Handler())
		t.Cleanup(server.Close)
		return server.URL
	}
	outOfScope := map[string]any{"match_id": "m_count", "players": []string{"p_1", "p_2"}}

	enforced := newServer(t, false)
	// Both counters exist from the first scrape, even before anything happened.
	body := scrapeMetrics(t, enforced)
	for _, want := range []string{
		`backend_service_scope_denied_total{service="platform"} 0`,
		`backend_service_scope_would_deny_total{service="platform"} 0`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("counter is not published at zero (%q):", want)
		}
	}
	doJSON(t, enforced, http.MethodPost, "/v1/session/sessions", outOfScope, "nakama-secret")
	if body := scrapeMetrics(t, enforced); !strings.Contains(body, `backend_service_scope_denied_total{service="platform"} 1`) {
		t.Fatalf("a refused call was not counted:\n%s", body)
	}

	observed := newServer(t, true)
	doJSON(t, observed, http.MethodPost, "/v1/session/sessions", outOfScope, "nakama-secret")
	body = scrapeMetrics(t, observed)
	if !strings.Contains(body, `backend_service_scope_would_deny_total{service="platform"} 1`) {
		t.Fatalf("a would-deny was not counted:\n%s", body)
	}
	if strings.Contains(body, `backend_service_scope_denied_total{service="platform"} 1`) {
		t.Fatalf("observe mode counted a real denial:\n%s", body)
	}
}

// TestOpLevelScopeIsEnforced is the op-level least-privilege contract: the Nakama bridge
// only exchanges identities, so its credential names `account.login` alone. It must be
// able to log in and must NOT be able to read or update a profile - a service-wide scope
// would have allowed both, which is what this narrows.
func TestOpLevelScopeIsEnforced(t *testing.T) {
	// Two service-auth ops in the same service: create is granted, mark_ready is not.
	// (Account is a poor demonstration: only login is service-auth there, the rest are
	// player-only, so a refusal would be 401 and prove nothing about scopes.)
	app, err := platform.New(platform.Config{
		Env:       "dev",
		DevAuth:   true,
		Allocator: "local",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ServiceCredentials: httpx.ServiceCredentials{
			{Name: "worker", Token: "worker-secret", Services: []string{"session.create"}},
		},
	})
	if err != nil {
		t.Fatalf("platform.New: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	status, body := doJSON(t, server.URL, http.MethodPost, "/v1/session/sessions",
		map[string]any{"match_id": "m_op_scope", "players": []string{"p_1", "p_2"}}, "worker-secret")
	if status != http.StatusOK {
		t.Fatalf("in-scope op = %d (%v), want 200", status, body)
	}
	sessionID, _ := body["session"].(map[string]any)["id"].(string)
	if sessionID == "" {
		t.Fatalf("create returned no session id: %v", body)
	}

	// mark_ready is in the same service, service-auth too, and not in the scope: 403.
	status, body = doJSON(t, server.URL, http.MethodPost,
		"/v1/session/sessions/"+sessionID+"/ready", map[string]any{"address": "1.2.3.4", "port": 7777}, "worker-secret")
	if status != http.StatusForbidden {
		t.Fatalf("sibling service-auth op = %d (%v), want 403", status, body)
	}
	if code, _ := body["error"].(map[string]any)["code"].(string); code != "permission_denied" {
		t.Fatalf("error code = %q, want permission_denied", code)
	}
}

// TestOpScopeTypoFailsAtStartup: an op id that does not exist can never match, so the
// credential would silently lose the permission it was written for. Startup is the only
// moment this is cheap to catch.
func TestOpScopeTypoFailsAtStartup(t *testing.T) {
	_, err := platform.New(platform.Config{
		Env:       "dev",
		DevAuth:   true,
		Allocator: "local",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		ServiceCredentials: httpx.ServiceCredentials{
			{Name: "nakama", Token: "nakama-secret", Services: []string{"account.logn"}},
		},
	})
	if err == nil {
		t.Fatal("a misspelled op id must fail platform startup")
	}
	if !strings.Contains(err.Error(), "account.logn") {
		t.Fatalf("error should name the bad entry: %v", err)
	}
}
