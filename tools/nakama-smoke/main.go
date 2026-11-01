// Command nakama-smoke drives the real Nakama → backend login bridge end to end:
//
//	device auth (Nakama)  →  RPC "login" (plugin)  →  backend POST /v1/account/sessions
//
// It is the acceptance test for M3 that cannot run on a machine without Docker, so
// CI runs it against the compose stack with the plugin mounted.
//
//	NAKAMA_HTTP=http://127.0.0.1:7350 NAKAMA_SERVER_KEY=defaultkey \
//	BACKEND_URL=http://127.0.0.1:8080 go run ./tools/nakama-smoke
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "nakama-smoke: %v\n", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

var httpClient = &http.Client{Timeout: 20 * time.Second}

func run() error {
	nakamaURL := env("NAKAMA_HTTP", "http://127.0.0.1:7350")
	serverKey := env("NAKAMA_SERVER_KEY", "defaultkey")
	backendURL := env("BACKEND_URL", "http://127.0.0.1:8080")
	deviceID := fmt.Sprintf("dsh-smoke-%d", time.Now().UnixNano())

	// 1. Authenticate a device account. This is Nakama's own login: the backend is
	// not involved yet, which is the point - Nakama owns identity.
	authBody, _ := json.Marshal(map[string]any{"id": deviceID})
	authReq, _ := http.NewRequest(http.MethodPost, nakamaURL+"/v2/account/authenticate/device?create=true",
		bytes.NewReader(authBody))
	authReq.Header.Set("Content-Type", "application/json")
	authReq.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(serverKey+":")))

	authResp, err := httpClient.Do(authReq)
	if err != nil {
		return fmt.Errorf("nakama device auth: %w", err)
	}
	authRaw, _ := readAll(authResp)
	if authResp.StatusCode != http.StatusOK {
		return fmt.Errorf("nakama device auth: HTTP %d: %s", authResp.StatusCode, truncate(authRaw))
	}
	var session struct {
		Token string `json:"token"`
		User  struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(authRaw, &session); err != nil {
		return fmt.Errorf("nakama device auth: decode: %w (keys: %v)", err, jsonKeys(authRaw))
	}
	if session.Token == "" {
		return fmt.Errorf("nakama device auth: no session token (keys: %v)", jsonKeys(authRaw))
	}
	// The user id is normally in the response body, but some server variants return
	// only the session; the token itself always carries it.
	if session.User.ID == "" {
		session.User.ID = externalIDFromJWT(session.Token)
	}
	if session.User.ID == "" {
		return fmt.Errorf("nakama device auth: no user id in the body or the token (keys: %v)",
			jsonKeys(authRaw))
	}
	fmt.Printf("nakama session ok: user=%s token_len=%d\n", session.User.ID, len(session.Token))

	// 2. Call the plugin's RPC. A 404 here means the plugin did not load, which is
	// the whole failure mode this smoke test exists to catch.
	rpcReq, _ := http.NewRequest(http.MethodPost, nakamaURL+"/v2/rpc/login?unwrap=true",
		strings.NewReader("{}"))
	rpcReq.Header.Set("Content-Type", "application/json")
	rpcReq.Header.Set("Authorization", "Bearer "+session.Token)

	rpcResp, err := httpClient.Do(rpcReq)
	if err != nil {
		return fmt.Errorf("rpc login: %w", err)
	}
	rpcRaw, _ := readAll(rpcResp)
	switch rpcResp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return fmt.Errorf("rpc login: HTTP 404 - the gamemodule plugin is not loaded in Nakama (check the .so build/ABI): %s", truncate(rpcRaw))
	default:
		return fmt.Errorf("rpc login: HTTP %d: %s", rpcResp.StatusCode, truncate(rpcRaw))
	}

	// The plugin returns the backend login response verbatim.
	var login struct {
		Player struct {
			ID         string `json:"id"`
			ExternalID string `json:"external_id"`
		} `json:"player"`
		SessionToken string `json:"session_token"`
	}
	if err := json.Unmarshal(rpcRaw, &login); err != nil {
		return fmt.Errorf("rpc login: decode %q: %w", truncate(rpcRaw), err)
	}
	if login.Player.ID == "" {
		return fmt.Errorf("rpc login: no player id in %s", truncate(rpcRaw))
	}
	if login.Player.ExternalID != session.User.ID {
		return fmt.Errorf("rpc login: external_id %q is not the Nakama user %q",
			login.Player.ExternalID, session.User.ID)
	}
	fmt.Printf("rpc login ok: player=%s external_id=%s\n", login.Player.ID, login.Player.ExternalID)

	// 3. Use the backend-issued token against the backend itself. This proves the
	// browser/client never needs a backend credential of its own.
	if login.SessionToken == "" {
		return fmt.Errorf("rpc login: plugin returned no session token")
	}
	profileReq, _ := http.NewRequest(http.MethodGet, backendURL+"/v1/account/players/"+login.Player.ID, nil)
	profileReq.Header.Set("Authorization", "Bearer "+login.SessionToken)
	profileResp, err := httpClient.Do(profileReq)
	if err != nil {
		return fmt.Errorf("backend profile read: %w", err)
	}
	profileRaw, _ := readAll(profileResp)
	if profileResp.StatusCode != http.StatusOK {
		return fmt.Errorf("backend profile read: HTTP %d: %s", profileResp.StatusCode, truncate(profileRaw))
	}
	fmt.Printf("backend read ok with the plugin-issued token (%d bytes)\n", len(profileRaw))

	// The external id is printed so the caller can assert the database row.
	fmt.Printf("EXTERNAL_ID=%s\n", session.User.ID)
	fmt.Printf("PLAYER_ID=%s\n", login.Player.ID)
	fmt.Println("OK: Nakama login → plugin RPC → backend account → backend read")
	return nil
}

// jsonKeys lists the top-level keys of a JSON object, for error messages that do not
// leak a session token (CI masks it in logs, which hides the rest of the body).
func jsonKeys(raw []byte) []string {
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil
	}
	keys := make([]string, 0, len(obj))
	for key := range obj {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// externalIDFromJWT reads the uid (or standard sub) claim without verifying the
// signature: the token came from Nakama over a trusted connection, and this is only a
// fallback for reading an id the body omitted.
func externalIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		UID string `json:"uid"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	if claims.UID != "" {
		return claims.UID
	}
	return claims.Sub
}

func readAll(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func truncate(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}
