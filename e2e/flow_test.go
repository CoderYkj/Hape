// Package e2e_test drives the whole backend over real HTTP: login, team, queue,
// allocation, battle report, fan-out and reads.
//
// This file is the executable definition of "the vertical slice works". If a
// refactor breaks the match loop, this test fails before a human notices.
package e2e_test

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"gamebackend/platform"
)

const serviceToken = "dev-service"

type client struct {
	t     *testing.T
	base  string
	token string
}

func newClient(t *testing.T, base, token string) *client {
	t.Helper()
	return &client{t: t, base: base, token: token}
}

func (c *client) do(method, path string, body any, headers ...string) (int, map[string]any) {
	c.t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			c.t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		c.t.Fatalf("read response: %v", err)
	}
	// Not every endpoint speaks JSON: /metrics is Prometheus text. Callers that
	// need a field walk the decoded body and fail loudly there.
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded
}

func expect(t *testing.T, got, want int, what string) {
	t.Helper()
	if got != want {
		t.Fatalf("%s: status = %d, want %d", what, got, want)
	}
}

// at walks a dotted path through decoded JSON, accepting list indices.
func at(t *testing.T, body map[string]any, path string) any {
	t.Helper()
	var current any = body
	for _, part := range strings.Split(path, ".") {
		switch node := current.(type) {
		case map[string]any:
			value, ok := node[part]
			if !ok {
				t.Fatalf("path %q: missing key %q", path, part)
			}
			current = value
		case []any:
			index, err := strconv.Atoi(part)
			if err != nil || index < 0 || index >= len(node) {
				t.Fatalf("path %q: %q is not a valid index", path, part)
			}
			current = node[index]
		default:
			t.Fatalf("path %q: cannot descend into %T at %q", path, current, part)
		}
	}
	return current
}

func atString(t *testing.T, body map[string]any, path string) string {
	t.Helper()
	value, ok := at(t, body, path).(string)
	if !ok {
		t.Fatalf("path %q is not a string", path)
	}
	return value
}

func atNumber(t *testing.T, body map[string]any, path string) int64 {
	t.Helper()
	value, ok := at(t, body, path).(float64)
	if !ok {
		t.Fatalf("path %q is not a number", path)
	}
	return int64(value)
}

func atBool(t *testing.T, body map[string]any, path string) bool {
	t.Helper()
	value, ok := at(t, body, path).(bool)
	if !ok {
		t.Fatalf("path %q is not a boolean", path)
	}
	return value
}

func newServer(t *testing.T) *httptest.Server {
	t.Helper()
	app, err := platform.New(platform.Config{
		Env:       "test",
		DevAuth:   true,
		Allocator: "local",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("platform.New: %v", err)
	}
	server := httptest.NewServer(app.Handler())
	t.Cleanup(func() {
		server.Close()
		_ = app.Close()
	})
	return server
}

func login(t *testing.T, server *httptest.Server, externalID string) (playerID, token string) {
	t.Helper()
	// ADR 0010: login is a service-only exchange. The client never calls it - the
	// Nakama plugin does, with a service credential.
	status, body := newClient(t, server.URL, serviceToken).do(http.MethodPost, "/v1/account/sessions",
		map[string]any{"external_id": externalID})
	expect(t, status, http.StatusOK, "login "+externalID)
	return atString(t, body, "player.id"), atString(t, body, "session_token")
}

// TestMatchLifecycle is the whole vertical slice in one readable test.
func TestMatchLifecycle(t *testing.T) {
	server := newServer(t)

	p1, token1 := login(t, server, "nakama-user-1")
	p2, token2 := login(t, server, "nakama-user-2")
	player1 := newClient(t, server.URL, token1)
	player2 := newClient(t, server.URL, token2)
	service := newClient(t, server.URL, serviceToken)

	// --- identity: a player may read only their own profile -----------------
	status, _ := newClient(t, server.URL, "").do(http.MethodGet, "/v1/account/players/"+p1, nil)
	expect(t, status, http.StatusUnauthorized, "anonymous profile read")

	status, body := player1.do(http.MethodGet, "/v1/account/players/"+p1, nil)
	expect(t, status, http.StatusOK, "own profile read")
	if got := atString(t, body, "player.id"); got != p1 {
		t.Fatalf("profile id = %q, want %q", got, p1)
	}

	status, _ = player1.do(http.MethodGet, "/v1/account/players/"+p2, nil)
	expect(t, status, http.StatusForbidden, "reading another player's profile")

	// --- team: join is idempotent ------------------------------------------
	status, body = player1.do(http.MethodPost, "/v1/team/teams", map[string]any{"name": "squad-one"})
	expect(t, status, http.StatusOK, "team create")
	teamID := atString(t, body, "team.id")

	status, body = player2.do(http.MethodPost, "/v1/team/teams/"+teamID+"/members", nil)
	expect(t, status, http.StatusOK, "team join")
	if !atBool(t, body, "applied") {
		t.Fatal("first join should apply")
	}

	status, body = player2.do(http.MethodPost, "/v1/team/teams/"+teamID+"/members", nil)
	expect(t, status, http.StatusOK, "team join replay")
	if atBool(t, body, "applied") {
		t.Fatal("second join must be a no-op")
	}

	// --- matchmaking: enqueue is idempotent per (queue, player) ------------
	status, body = player1.do(http.MethodPost, "/v1/matchmaking/tickets", map[string]any{"queue": "ranked_1v1"})
	expect(t, status, http.StatusOK, "enqueue p1")
	ticket1 := atString(t, body, "ticket.id")

	status, body = player1.do(http.MethodPost, "/v1/matchmaking/tickets", map[string]any{"queue": "ranked_1v1"})
	expect(t, status, http.StatusOK, "enqueue p1 replay")
	if got := atString(t, body, "ticket.id"); got != ticket1 {
		t.Fatalf("re-enqueue created ticket %q, want %q", got, ticket1)
	}

	status, _ = player2.do(http.MethodPost, "/v1/matchmaking/tickets", map[string]any{"queue": "ranked_1v1"})
	expect(t, status, http.StatusOK, "enqueue p2")

	// --- a player may not dequeue: the matchmaker holds the service token ---
	status, _ = player1.do(http.MethodPost, "/v1/matchmaking/queues/ranked_1v1/pop", map[string]any{"count": 2})
	expect(t, status, http.StatusUnauthorized, "player attempting to pop the queue")

	status, body = service.do(http.MethodPost, "/v1/matchmaking/queues/ranked_1v1/pop", map[string]any{"count": 2})
	expect(t, status, http.StatusOK, "matchmaker pop")
	if !atBool(t, body, "matched") {
		t.Fatal("pop should have formed a match")
	}
	matchID := atString(t, body, "match_id")

	// --- session: allocate, ready, report ---------------------------------
	status, body = service.do(http.MethodPost, "/v1/session/sessions", map[string]any{
		"match_id": matchID,
		"players":  []string{p1, p2},
		"queue":    "ranked_1v1",
		"map":      "arena_01",
	})
	expect(t, status, http.StatusOK, "session create")
	sessionID := atString(t, body, "session.id")
	// Dual credentials: the DS authenticates as a service AND as this session.
	dsToken := atString(t, body, "runtime_token")
	if dsToken == "" {
		t.Fatal("session.create must return the per-session runtime token")
	}
	if state := atString(t, body, "session.state"); state != "allocating" {
		t.Fatalf("session state = %q, want allocating", state)
	}
	if provider := atString(t, body, "session.provider"); provider != "local" {
		t.Fatalf("provider = %q, want local", provider)
	}

	status, body = service.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/ready", map[string]any{
		"address": "10.0.0.7",
		"port":    7777,
	}, "X-Session-Token", dsToken)
	expect(t, status, http.StatusOK, "session ready")
	if state := atString(t, body, "session.state"); state != "ready" {
		t.Fatalf("session state = %q, want ready", state)
	}

	// A client cannot report results; only the dedicated server may.
	status, _ = player1.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/results", map[string]any{
		"result_id": "forged",
		"players":   []map[string]any{{"player_id": p1, "won": true}, {"player_id": p2, "won": false}},
	})
	expect(t, status, http.StatusUnauthorized, "client forging a battle report")

	resultBody := map[string]any{
		"result_id":        "result-1",
		"duration_seconds": 312,
		"players": []map[string]any{
			{"player_id": p1, "won": true, "score": 900, "kills": 9, "deaths": 2, "damage": 1800},
			{"player_id": p2, "won": false, "score": 400, "kills": 2, "deaths": 9, "damage": 700},
		},
	}
	status, body = service.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/results", resultBody,
		"X-Session-Token", dsToken)
	expect(t, status, http.StatusOK, "battle report")
	if !atBool(t, body, "applied") {
		t.Fatal("first battle report must apply")
	}

	// --- replay is idempotent: no double payout ---------------------------
	status, body = service.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/results", resultBody,
		"X-Session-Token", dsToken)
	expect(t, status, http.StatusOK, "battle report replay")
	if atBool(t, body, "applied") {
		t.Fatal("replayed battle report must not apply again")
	}

	// Without the session token the report is refused even with a valid service
	// token: the shared token proves "a trusted process", not "this match's DS".
	status, _ = service.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/results", resultBody)
	expect(t, status, http.StatusUnauthorized, "report without the session token")

	// A different result for the same session is a conflict, not an overwrite.
	other := map[string]any{"result_id": "result-2", "players": resultBody["players"]}
	status, _ = service.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/results", other,
		"X-Session-Token", dsToken)
	expect(t, status, http.StatusConflict, "second distinct result")

	// --- fan-out: economy, progress and rank all saw the match once --------
	status, body = player1.do(http.MethodGet, "/v1/economy/players/"+p1+"/wallet", nil)
	expect(t, status, http.StatusOK, "wallet read")
	if gold := atNumber(t, body, "wallet.balances.gold"); gold != 100 {
		t.Fatalf("gold = %d, want 100", gold)
	}

	status, body = player1.do(http.MethodGet, "/v1/progress/players/"+p1, nil)
	expect(t, status, http.StatusOK, "progress read")
	if xp := atNumber(t, body, "progress.total_xp"); xp != 140 {
		t.Fatalf("total_xp = %d, want 140 (50 + 900/10)", xp)
	}

	status, body = player1.do(http.MethodGet, "/v1/rank/leaderboards/ranked_1v1", nil)
	expect(t, status, http.StatusOK, "leaderboard read")
	if rating := atNumber(t, body, "leaderboard.entries.0.rating"); rating != 1024 {
		t.Fatalf("winner rating = %d, want 1024", rating)
	}
	if winner := atString(t, body, "leaderboard.entries.0.player_id"); winner != p1 {
		t.Fatalf("leaderboard leader = %q, want %q", winner, p1)
	}

	// --- the session is finished and visible to its players ---------------
	status, body = player1.do(http.MethodGet, "/v1/session/sessions/"+sessionID, nil)
	expect(t, status, http.StatusOK, "session read")
	if state := atString(t, body, "session.state"); state != "finished" {
		t.Fatalf("session state = %q, want finished", state)
	}

	// --- economy invariants ----------------------------------------------
	status, _ = player1.do(http.MethodPost, "/v1/economy/players/"+p1+"/purchases",
		map[string]any{"currency": "gems", "amount": 100, "item_id": "skin-01"},
		"Idempotency-Key", "purchase-1")
	expect(t, status, http.StatusPreconditionFailed, "spending currency the player does not own")

	status, body = player1.do(http.MethodPost, "/v1/economy/players/"+p1+"/purchases",
		map[string]any{"currency": "gold", "amount": 60, "item_id": "skin-01"},
		"Idempotency-Key", "purchase-2")
	expect(t, status, http.StatusOK, "buying a skin")
	if gold := atNumber(t, body, "wallet.balances.gold"); gold != 40 {
		t.Fatalf("gold after purchase = %d, want 40", gold)
	}

	status, body = player1.do(http.MethodGet, "/v1/economy/players/"+p1+"/ledger", nil)
	expect(t, status, http.StatusOK, "ledger read")
	entries, ok := at(t, body, "entries").([]any)
	if !ok || len(entries) != 2 {
		t.Fatalf("ledger entries = %v, want 2 movements", at(t, body, "entries"))
	}
}

// TestErrorContract pins the error envelope: one shape, one code vocabulary.
func TestErrorContract(t *testing.T) {
	server := newServer(t)
	anon := newClient(t, server.URL, "")

	// Anonymous callers are rejected before the body is even looked at.
	status, body := anon.do(http.MethodPost, "/v1/account/sessions", map[string]any{"external_id": "someone"})
	expect(t, status, http.StatusUnauthorized, "anonymous login")
	if code := atString(t, body, "error.code"); code != "unauthenticated" {
		t.Fatalf("anonymous login: code = %q, want unauthenticated", code)
	}

	service := newClient(t, server.URL, serviceToken)
	status, body = service.do(http.MethodPost, "/v1/account/sessions", map[string]any{"bogus_field": "x"})
	expect(t, status, http.StatusBadRequest, "unknown field")
	if code := atString(t, body, "error.code"); code != "invalid_argument" {
		t.Fatalf("error code = %q, want invalid_argument", code)
	}

	_, token := login(t, server, "nakama-user-3")
	player := newClient(t, server.URL, token)

	status, _ = service.do(http.MethodPost, "/v1/account/sessions", map[string]any{})
	expect(t, status, http.StatusBadRequest, "missing external_id")

	// A player token is not a service credential.
	status, _ = player.do(http.MethodPost, "/v1/account/sessions", map[string]any{"external_id": "someone"})
	expect(t, status, http.StatusUnauthorized, "login with a player token")

	status, body = player.do(http.MethodGet, "/v1/account/players/p_missing", nil)
	expect(t, status, http.StatusForbidden, "reading a stranger's profile")
	if code := atString(t, body, "error.code"); code != "permission_denied" {
		t.Fatalf("error code = %q, want permission_denied", code)
	}

	// Writes that need an idempotency key must say so.
	status, body = service.do(http.MethodPost, "/v1/progress/players/p_1/xp", map[string]any{"xp": 10, "reason": "test"})
	expect(t, status, http.StatusBadRequest, "grant without Idempotency-Key")
	if code := atString(t, body, "error.code"); code != "invalid_argument" {
		t.Fatalf("error code = %q, want invalid_argument", code)
	}
}

// TestHealthz keeps the probe contract honest.
func TestHealthz(t *testing.T) {
	server := newServer(t)
	anon := newClient(t, server.URL, "")
	for _, path := range []string{"/healthz", "/readyz", "/metrics"} {
		status, _ := anon.do(http.MethodGet, path, nil)
		expect(t, status, http.StatusOK, path)
	}
}
