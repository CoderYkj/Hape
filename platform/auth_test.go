package platform_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

	// Login is a public op: the Nakama plugin exchanges a verified identity for a
	// backend player. (KNOWN GAP: it trusts the external_id in the body - see
	// README "known gaps" - so this step is only safe behind the plugin today.)
	status, body := doJSON(t, server.URL, http.MethodPost, "/v1/account/sessions",
		map[string]any{"external_id": "nakama-user-9"}, "")
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

	// A valid token gets the player's own profile.
	status, body = doJSON(t, server.URL, http.MethodGet, "/v1/account/players/"+playerID, nil,
		token("nakama-user-9", []byte(key), time.Hour))
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
