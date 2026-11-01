// Command minimal is the client half of the login path, in the smallest form that can
// be verified end to end. It performs exactly the sequence a UE5 client performs:
//
//  1. authenticate with Nakama (device)          -> Nakama session token
//  2. call the bridge RPC "login"                -> backend player id + backend token
//  3. read its own profile with the backend token
//  4. enqueue for a match                        -> waiting ticket
//  5. read the ticket back                       -> still waiting
//
// It is deliberately a plain HTTP program: the point is to pin the *protocol* the
// engine has to speak, not to be the engine. docs/ue5-client-model.md maps each step
// onto UE5's HTTP/JSON APIs, and the CI job `nakama` runs this against a live stack.
//
//	NAKAMA_HTTP=http://127.0.0.1:7350 BACKEND_URL=http://127.0.0.1:8080 \
//	  go run ./clients/minimal
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

var client = &http.Client{Timeout: 20 * time.Second}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "minimal-client: %v\n", err)
		os.Exit(1)
	}
}

func env(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func run() error {
	nakamaURL := env("NAKAMA_HTTP", "http://127.0.0.1:7350")
	serverKey := env("NAKAMA_SERVER_KEY", "defaultkey")
	backendURL := env("BACKEND_URL", "http://127.0.0.1:8080")
	queue := env("QUEUE", "ranked_1v1")
	deviceID := fmt.Sprintf("ue5-minimal-%d", time.Now().UnixNano())

	// Step 1: Nakama owns identity. A real client uses device/Steam/Apple auth; the
	// shape of what comes back is the same.
	sessionToken, nakamaUser, err := nakamaDeviceAuth(nakamaURL, serverKey, deviceID)
	if err != nil {
		return fmt.Errorf("step 1 (nakama auth): %w", err)
	}
	fmt.Printf("1. nakama session: user=%s (token %d chars)\n", nakamaUser, len(sessionToken))

	// Step 2: exchange that session for a backend identity through the runtime RPC.
	playerID, backendToken, err := bridgeLogin(nakamaURL, sessionToken)
	if err != nil {
		return fmt.Errorf("step 2 (rpc login): %w", err)
	}
	fmt.Printf("2. backend identity: player=%s (token %d chars)\n", playerID, len(backendToken))

	// Step 3: every subsequent call uses the BACKEND token, not the Nakama one. The
	// client never holds a backend credential of its own.
	status, body, err := backendRequest(backendURL+"/v1/account/players/"+playerID, http.MethodGet, nil, backendToken)
	if err != nil {
		return fmt.Errorf("step 3 (profile): %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 3 (profile): HTTP %d: %s", status, trim(body))
	}
	fmt.Printf("3. profile read: %d bytes\n", len(body))

	// Step 4: enqueue. The client chooses a queue; the server decides the match.
	payload, _ := json.Marshal(map[string]any{"queue": queue})
	status, body, err = backendRequest(backendURL+"/v1/matchmaking/tickets", http.MethodPost, payload, backendToken)
	if err != nil {
		return fmt.Errorf("step 4 (enqueue): %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 4 (enqueue): HTTP %d: %s", status, trim(body))
	}
	ticketID := dig(body, "ticket", "id")
	if ticketID == "" {
		return fmt.Errorf("step 4 (enqueue): no ticket id in %s", trim(body))
	}
	fmt.Printf("4. enqueued: ticket=%s\n", ticketID)

	// Step 5: poll the ticket. A client waits here (or polls its session); it never
	// pops the queue and never reports a result - both are service/DS operations.
	status, body, err = backendRequest(backendURL+"/v1/matchmaking/tickets/"+ticketID, http.MethodGet, nil, backendToken)
	if err != nil {
		return fmt.Errorf("step 5 (poll): %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 5 (poll): HTTP %d: %s", status, trim(body))
	}
	state := dig(body, "ticket", "status")
	if state != "waiting" {
		return fmt.Errorf("step 5 (poll): ticket status = %q, want waiting", state)
	}
	fmt.Printf("5. polled: ticket status=%s\n", state)

	// Printed for the CI assertions.
	fmt.Printf("NAKAMA_USER=%s\n", nakamaUser)
	fmt.Printf("PLAYER_ID=%s\n", playerID)
	fmt.Printf("TICKET_ID=%s\n", ticketID)
	fmt.Println("CLIENT_OK=1")
	fmt.Println("OK: nakama login -> rpc -> backend profile -> enqueue -> poll")
	return nil
}

func nakamaDeviceAuth(base, serverKey, deviceID string) (token, userID string, err error) {
	body, _ := json.Marshal(map[string]any{"id": deviceID})
	req, err := http.NewRequest(http.MethodPost, base+"/v2/account/authenticate/device?create=true", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(serverKey+":")))

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	raw, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, trim(raw))
	}
	var decoded struct {
		Token string `json:"token"`
		User  struct {
			ID string `json:"id"`
		} `json:"user"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", "", fmt.Errorf("decode: %w", err)
	}
	userID = decoded.User.ID
	if userID == "" {
		userID = claim(decoded.Token, "uid", "sub")
	}
	if decoded.Token == "" || userID == "" {
		return "", "", fmt.Errorf("no session token or user id")
	}
	return decoded.Token, userID, nil
}

func bridgeLogin(base, sessionToken string) (playerID, backendToken string, err error) {
	req, err := http.NewRequest(http.MethodPost, base+"/v2/rpc/login?unwrap=true", strings.NewReader("{}"))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+sessionToken)

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	raw, _ := readAll(resp)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, trim(raw))
	}
	var decoded struct {
		Player struct {
			ID string `json:"id"`
		} `json:"player"`
		SessionToken string `json:"session_token"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return "", "", fmt.Errorf("decode %s: %w", trim(raw), err)
	}
	if decoded.Player.ID == "" || decoded.SessionToken == "" {
		return "", "", fmt.Errorf("bridge returned no player id or token: %s", trim(raw))
	}
	return decoded.Player.ID, decoded.SessionToken, nil
}

func backendRequest(url, method string, body []byte, token string) (int, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	raw, err := readAll(resp)
	return resp.StatusCode, raw, err
}

// dig walks nested JSON objects and returns a string field ("" when absent).
func dig(raw []byte, keys ...string) string {
	var current any
	if err := json.Unmarshal(raw, &current); err != nil {
		return ""
	}
	for _, key := range keys {
		obj, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = obj[key]
	}
	value, _ := current.(string)
	return value
}

// claim reads a JWT payload field without verifying it: the token came from Nakama
// over a trusted local connection and this is only an id fallback.
func claim(token string, keys ...string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	for _, key := range keys {
		if v, ok := claims[key].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func readAll(resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}

func trim(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}
