// Command minimal is the client half of the match loop, in the smallest form that can
// be verified end to end. It performs exactly the sequence a UE5 client performs:
//
//  1. authenticate (Nakama device auth, or a dev identity)  -> backend token
//  2. read its own profile with that token
//  3. enqueue for a match                                    -> waiting ticket
//  4. read the ticket back                                   -> waiting (or matched)
//  5. wait for the matchmaker to pair it                    -> match id
//  6. find the server for that match                        -> address:port
//  7. open a socket to it and exchange packets              -> echoes
//  8. wait for the authoritative result, then read the payout
//
// Steps 5-8 need a provider on the other side (tools/fakeds -mode worker). They are
// therefore opt-in (CLIENT_FULL=1) so the default run stays a five-step protocol check
// that needs nothing but a backend.
//
// It is deliberately a plain HTTP/UDP program: the point is to pin the *protocol* the
// engine has to speak, not to be the engine. docs/ue5-client-model.md maps each step
// onto UE5's HTTP/JSON APIs.
//
//	NAKAMA_HTTP=http://127.0.0.1:7350 BACKEND_URL=http://127.0.0.1:8080 \
//	  go run ./clients/minimal
//
//	# the full loop, without Nakama (dev identity, a bot opponent, a real server)
//	IDENTITY=dev CLIENT_FULL=1 QUEUE=ue5-drill go run ./clients/minimal
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

// identity is what the client proves on every backend call. It never contains a
// Nakama token: Nakama is exchanged once, then only the backend token is used.
type identity struct {
	userID       string
	playerID     string
	backendToken string
}

// caller is one authenticated backend session plus the recipe for renewing it.
//
// Player tokens expire (BACKEND_PLAYER_TOKEN_TTL, default 1h). A client that treats a 401
// as fatal dies mid-match, so "re-authenticate and replay once" is part of the protocol the
// engine has to implement - and this type is where that lives, so every call gets it
// instead of only the one path someone remembered.
type caller struct {
	baseURL string
	token   string
	// relogin re-does step 1 and returns a fresh backend token. Nil means the token cannot
	// be renewed, in which case a 401 is returned as-is.
	relogin func() (string, error)
}

// do issues one call, and on 401 renews the token and replays the request exactly once.
//
// Once, not in a loop: if the fresh token is also rejected, the problem is not expiry and
// retrying makes an outage look like a slow but working client.
func (c *caller) do(method, path string, body []byte) (int, []byte, error) {
	status, resp, err := backendRequest(c.baseURL+path, method, body, c.token)
	if err != nil || status != http.StatusUnauthorized || c.relogin == nil {
		return status, resp, err
	}
	token, err := c.relogin()
	if err != nil {
		return status, resp, fmt.Errorf("re-authentication failed: %w", err)
	}
	c.token = token
	return backendRequest(c.baseURL+path, method, body, token)
}

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

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv(key))); err == nil && v > 0 {
		return v
	}
	return fallback
}

func run() error {
	backendURL := env("BACKEND_URL", "http://127.0.0.1:8080")
	nakamaURL := env("NAKAMA_HTTP", "http://127.0.0.1:7350")
	serverKey := env("NAKAMA_SERVER_KEY", "defaultkey")
	queue := env("QUEUE", "ranked_1v1")
	identityMode := env("IDENTITY", "nakama")
	full := env("CLIENT_FULL", "0") == "1"
	bot := env("CLIENT_BOT", "1") == "1"
	wait := time.Duration(envInt("MATCH_TIMEOUT_SECONDS", 60)) * time.Second
	packets := envInt("INPUT_PACKETS", 5)

	deviceID := fmt.Sprintf("ue5-minimal-%d", time.Now().UnixNano())

	// Step 1: identity. Nakama owns it in production; the dev mode creates identities
	// directly so the match loop can be drilled without a second service running.
	me, err := signIn(identityMode, nakamaURL, serverKey, backendURL, deviceID)
	if err != nil {
		return fmt.Errorf("step 1 (sign in): %w", err)
	}
	fmt.Printf("1. identity: player=%s (token %d chars)\n", me.playerID, len(me.backendToken))

	// Every request below goes through this caller, so an expired token is renewed once
	// and the request replayed instead of failing the run.
	client := &caller{baseURL: backendURL, token: me.backendToken}
	client.relogin = func() (string, error) {
		fresh, err := signIn(identityMode, nakamaURL, serverKey, backendURL, deviceID)
		if err != nil {
			return "", err
		}
		return fresh.backendToken, nil
	}

	// Step 2: every subsequent call uses the backend token, and the client holds no
	// credential of its own beyond it.
	status, body, err := client.do(http.MethodGet, "/v1/account/players/"+me.playerID, nil)
	if err != nil {
		return fmt.Errorf("step 2 (profile): %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 2 (profile): HTTP %d: %s", status, trim(body))
	}
	fmt.Printf("2. profile read: %d bytes\n", len(body))

	// Step 3: enqueue. The client chooses a queue; the server decides the match.
	ticketID, err := enqueue(client, queue)
	if err != nil {
		return fmt.Errorf("step 3 (enqueue): %w", err)
	}
	fmt.Printf("3. enqueued: ticket=%s\n", ticketID)

	// Step 4: poll the ticket. A client never pops the queue and never reports a
	// result - both are service/DS operations.
	status, body, err = client.do(http.MethodGet, "/v1/matchmaking/tickets/"+ticketID, nil)
	if err != nil {
		return fmt.Errorf("step 4 (poll): %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 4 (poll): HTTP %d: %s", status, trim(body))
	}
	state := dig(body, "ticket", "status")
	if state != "waiting" && state != "matched" {
		return fmt.Errorf("step 4 (poll): ticket status = %q, want waiting or matched", state)
	}
	fmt.Printf("4. polled: ticket status=%s\n", state)

	fmt.Printf("PLAYER_ID=%s\n", me.playerID)
	fmt.Printf("TICKET_ID=%s\n", ticketID)

	if !full {
		fmt.Printf("NAKAMA_USER=%s\n", me.userID)
		fmt.Println("CLIENT_OK=1")
		fmt.Println("OK: identity -> profile -> enqueue -> poll")
		return nil
	}

	// Step 5: a second identity so the queue can produce a match. In production this is
	// another player; here it is a bot that enqueues and then does nothing, which is all
	// the matchmaker needs.
	if bot {
		opponent, err := signIn(identityMode, nakamaURL, serverKey, backendURL, deviceID+"-bot")
		if err != nil {
			return fmt.Errorf("step 5 (bot sign in): %w", err)
		}
		if _, err := enqueue(&caller{baseURL: backendURL, token: opponent.backendToken}, queue); err != nil {
			return fmt.Errorf("step 5 (bot enqueue): %w", err)
		}
		fmt.Printf("5. opponent queued: player=%s\n", opponent.playerID)
		fmt.Printf("BOT_PLAYER_ID=%s\n", opponent.playerID)
	}

	// Step 6: wait for the matchmaker. The client learns a match id and nothing else.
	matchID, err := waitForMatch(client, ticketID, wait)
	if err != nil {
		return fmt.Errorf("step 6 (wait for match): %w", err)
	}
	fmt.Printf("6. matched: match=%s\n", matchID)
	fmt.Printf("MATCH_ID=%s\n", matchID)

	// Step 7: resolve the match to the server. This is the read path that used to be
	// missing: a ticket carries a match id, and the client needs an address.
	sessionID, address, port, err := waitForSession(client, matchID, wait)
	if err != nil {
		return fmt.Errorf("step 7 (find the server): %w", err)
	}
	fmt.Printf("7. server: session=%s address=%s:%d\n", sessionID, address, port)
	fmt.Printf("SESSION_ID=%s\n", sessionID)
	fmt.Printf("SERVER_ADDRESS=%s:%d\n", address, port)

	// Step 8: connect. Everything above is HTTP; this is the part a UE5 client cannot
	// skip, and the part no mock can stand in for.
	echoes, err := udpRoundTrip(address, port, packets, 5*time.Second)
	if err != nil {
		return fmt.Errorf("step 8 (connect): %w", err)
	}
	fmt.Printf("8. round trip: %d/%d packets echoed back\n", echoes, packets)
	fmt.Printf("ECHOES=%d\n", echoes)
	if echoes != packets {
		return fmt.Errorf("step 8 (connect): %d of %d packets came back", echoes, packets)
	}

	// Step 9: the server is authoritative. The client waits for `finished`, then reads
	// its own payout with its own token - it never learns the result any other way.
	finalState, err := waitForFinished(client, matchID, wait)
	if err != nil {
		return fmt.Errorf("step 9 (result): %w", err)
	}
	fmt.Printf("9. session state: %s\n", finalState)
	fmt.Printf("SESSION_STATE=%s\n", finalState)

	status, body, err = client.do(http.MethodGet, "/v1/progress/players/"+me.playerID, nil)
	if err != nil {
		return fmt.Errorf("step 9 (read payout): %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 9 (read payout): HTTP %d: %s", status, trim(body))
	}
	xp := digNumber(body, "progress", "total_xp")
	fmt.Printf("10. payout: total_xp=%d\n", int64(xp))
	if xp <= 0 {
		return fmt.Errorf("step 10 (payout): total_xp = %v, the match settled nothing", xp)
	}

	fmt.Printf("NAKAMA_USER=%s\n", me.userID)
	fmt.Printf("TOTAL_XP=%d\n", int64(xp))
	fmt.Println("CLIENT_OK=1")
	fmt.Println("OK: identity -> profile -> enqueue -> match -> server -> packets -> payout")
	return nil
}

// signIn produces a backend identity. Nakama is the production path; "dev" creates the
// account directly so the match loop can be drilled without a second service.
func signIn(mode, nakamaURL, serverKey, backendURL, deviceID string) (identity, error) {
	switch mode {
	case "dev":
		payload, _ := json.Marshal(map[string]any{"external_id": deviceID})
		status, body, err := backendRequest(backendURL+"/v1/account/sessions", http.MethodPost, payload, "dev-service")
		if err != nil {
			return identity{}, err
		}
		if status != http.StatusOK {
			return identity{}, fmt.Errorf("dev sign in: HTTP %d: %s", status, trim(body))
		}
		playerID := dig(body, "player", "id")
		token := dig(body, "session_token")
		if playerID == "" || token == "" {
			return identity{}, fmt.Errorf("dev sign in returned no player or token: %s", trim(body))
		}
		return identity{playerID: playerID, backendToken: token}, nil
	case "nakama":
		sessionToken, userID, err := nakamaDeviceAuth(nakamaURL, serverKey, deviceID)
		if err != nil {
			return identity{}, err
		}
		playerID, backendToken, err := bridgeLogin(nakamaURL, sessionToken)
		if err != nil {
			return identity{}, err
		}
		return identity{userID: userID, playerID: playerID, backendToken: backendToken}, nil
	default:
		return identity{}, fmt.Errorf("unknown IDENTITY %q (want nakama or dev)", mode)
	}
}

func enqueue(c *caller, queue string) (string, error) {
	payload, _ := json.Marshal(map[string]any{"queue": queue})
	status, body, err := c.do(http.MethodPost, "/v1/matchmaking/tickets", payload)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("HTTP %d: %s", status, trim(body))
	}
	ticketID := dig(body, "ticket", "id")
	if ticketID == "" {
		return "", fmt.Errorf("no ticket id in %s", trim(body))
	}
	return ticketID, nil
}

// waitForMatch polls the ticket until the matchmaker pairs it.
func waitForMatch(c *caller, ticketID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		status, body, err := c.do(http.MethodGet, "/v1/matchmaking/tickets/"+ticketID, nil)
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", fmt.Errorf("HTTP %d: %s", status, trim(body))
		}
		if dig(body, "ticket", "status") == "matched" {
			if matchID := dig(body, "ticket", "match_id"); matchID != "" {
				return matchID, nil
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no match after %s (is a provider running? tools/fakeds -mode worker)", timeout)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// waitForSession resolves the match to a listening server.
func waitForSession(c *caller, matchID string, timeout time.Duration) (string, string, int, error) {
	deadline := time.Now().Add(timeout)
	lastState := ""
	for {
		status, body, err := c.do(http.MethodGet, "/v1/session/sessions/by-match/"+matchID, nil)
		if err != nil {
			return "", "", 0, err
		}
		switch status {
		case http.StatusOK:
			lastState = dig(body, "session", "state")
			address := dig(body, "session", "address")
			port := int(digNumber(body, "session", "port"))
			if (lastState == "ready" || lastState == "in_progress") && address != "" && port != 0 {
				return dig(body, "session", "id"), address, port, nil
			}
			if lastState == "failed" {
				return "", "", 0, fmt.Errorf("the server allocation failed")
			}
		case http.StatusNotFound:
			lastState = "not created yet"
		default:
			return "", "", 0, fmt.Errorf("HTTP %d: %s", status, trim(body))
		}
		if time.Now().After(deadline) {
			return "", "", 0, fmt.Errorf("no server after %s (session state: %s)", timeout, lastState)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// waitForFinished waits for the dedicated server's report to land.
func waitForFinished(c *caller, matchID string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	state := ""
	for {
		status, body, err := c.do(http.MethodGet, "/v1/session/sessions/by-match/"+matchID, nil)
		if err != nil {
			return "", err
		}
		if status == http.StatusOK {
			state = dig(body, "session", "state")
			if state == "finished" {
				return state, nil
			}
			if state == "failed" {
				return "", fmt.Errorf("session failed before finishing")
			}
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("session still %q after %s", state, timeout)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// udpRoundTrip is the actual connection: send a packet, read the echo, repeat.
//
// The reference server echoes; a UE5 server replicates. Either way the client can only
// prove the link by getting bytes back, never by trusting a log or a status field.
func udpRoundTrip(address string, port, packets int, timeout time.Duration) (int, error) {
	ip := net.ParseIP(address)
	if ip == nil {
		return 0, fmt.Errorf("server address %q is not an IP", address)
	}
	conn, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return 0, err
	}
	defer func() { _ = conn.Close() }()

	echoes := 0
	buf := make([]byte, 2048)
	for i := 0; i < packets; i++ {
		payload := []byte(fmt.Sprintf(`{"input":%d,"tick":%d}`, i, time.Now().UnixNano()))
		if _, err := conn.Write(payload); err != nil {
			return echoes, err
		}
		_ = conn.SetReadDeadline(time.Now().Add(timeout))
		n, err := conn.Read(buf)
		if err != nil {
			return echoes, fmt.Errorf("packet %d: %w", i, err)
		}
		if bytes.Equal(buf[:n], payload) {
			echoes++
		}
	}
	return echoes, nil
}

func nakamaDeviceAuth(base, serverKey, deviceID string) (token, userID string, err error) {
	body, _ := json.Marshal(map[string]any{"id": deviceID})
	req, err := http.NewRequest(http.MethodPost, base+"/v2/account/authenticate/device?create=true", bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(serverKey+":")))

	resp, err := httpClient.Do(req)
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

	resp, err := httpClient.Do(req)
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
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	raw, err := readAll(resp)
	return resp.StatusCode, raw, err
}

// dig walks nested JSON objects and returns a string field ("" when absent).
func dig(raw []byte, keys ...string) string {
	if value := digAny(raw, keys...); value != nil {
		text, _ := value.(string)
		return text
	}
	return ""
}

// digNumber is dig for a numeric field (0 when absent).
func digNumber(raw []byte, keys ...string) float64 {
	if value := digAny(raw, keys...); value != nil {
		number, _ := value.(float64)
		return number
	}
	return 0
}

func digAny(raw []byte, keys ...string) any {
	var current any
	if err := json.Unmarshal(raw, &current); err != nil {
		return nil
	}
	for _, key := range keys {
		obj, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = obj[key]
	}
	return current
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
