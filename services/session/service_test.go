// Package session tests the lifecycle edges that e2e/flow_test.go cannot reach:
// provider failures, fan-out failures and illegal transitions. The happy path
// lives in e2e; this file pins the failure modes declared in
// .harness/specs/0001-m0-vertical-slice.spec.md section 6.
package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"gamebackend/pkg/battlealloc"
	"gamebackend/pkg/httpx"
)

const serviceToken = "dev-service"

// fakeAllocator fails on demand, so "the provider is out of capacity" is a
// one-line switch instead of a vendor account.
type fakeAllocator struct {
	fail bool
}

func (f *fakeAllocator) Provider() string { return "fake" }

func (f *fakeAllocator) Allocate(context.Context, battlealloc.Request) (battlealloc.Allocation, error) {
	if f.fail {
		return battlealloc.Allocation{}, errors.New("provider quota exceeded")
	}
	return battlealloc.Allocation{ID: "alloc-1", Provider: "fake", Address: "10.0.0.9", Port: 7777}, nil
}

func (f *fakeAllocator) Release(context.Context, string) error { return nil }

// fakeSink is the fan-out boundary: it can be broken and then repaired, which
// is exactly the retry story the contract promises.
type fakeSink struct {
	fail  bool
	calls int
}

func (f *fakeSink) MatchFinished(context.Context, FinishedMatch) error {
	f.calls++
	if f.fail {
		return errors.New("fan-out bus unavailable")
	}
	return nil
}

type harness struct {
	t    *testing.T
	srv  *httptest.Server
	app  *Service
	auth string
}

func newHarness(t *testing.T, alloc battlealloc.Allocator, sink ResultSink) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpx.NewRouter("session", log, httpx.NewDevAuth(serviceToken))
	app := NewWithStore(log, NewMemory(), alloc, sink)
	app.Register(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &harness{t: t, srv: server, app: app, auth: serviceToken}
}

// do issues one request and returns the status plus the decoded body (nil for
// non-JSON responses, which this package never produces).
func (h *harness) do(method, path string, body any, token string) (int, map[string]any) {
	h.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			h.t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, reader)
	if err != nil {
		h.t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	var decoded map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp.StatusCode, decoded
}

func (h *harness) expect(got, want int, what string) {
	h.t.Helper()
	if got != want {
		h.t.Fatalf("%s: status = %d, want %d", what, got, want)
	}
}

func nested(t *testing.T, body map[string]any, keys ...string) any {
	t.Helper()
	var current any = body
	for _, key := range keys {
		node, ok := current.(map[string]any)
		if !ok {
			t.Fatalf("cannot descend into %T looking for %q", current, key)
		}
		value, ok := node[key]
		if !ok {
			t.Fatalf("missing key %q", key)
		}
		current = value
	}
	return current
}

// createSession creates and (optionally) readies a session, returning its id.
func (h *harness) createSession(players ...string) string {
	h.t.Helper()
	status, body := h.do(http.MethodPost, "/v1/session/sessions", map[string]any{
		"match_id": "m_test",
		"players":  players,
		"queue":    "ranked_1v1",
	}, h.auth)
	h.expect(status, http.StatusOK, "session create")
	id, ok := nested(h.t, body, "session", "id").(string)
	if !ok {
		h.t.Fatal("session create: no id in response")
	}
	return id
}

func (h *harness) ready(id string) {
	h.t.Helper()
	status, _ := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/ready", map[string]any{}, h.auth)
	h.expect(status, http.StatusOK, "session ready")
}

func (h *harness) report(id string) map[string]any {
	h.t.Helper()
	return map[string]any{
		"result_id":        "result-" + id,
		"duration_seconds": 300,
		"players": []map[string]any{
			{"player_id": "p_1", "won": true, "score": 500},
			{"player_id": "p_2", "won": false, "score": 100},
		},
	}
}

// TestAllocationFailureFailsTheSession pins spec section 6: allocation failure
// is a 503 that leaves the session permanently failed, never a half-open one.
func TestAllocationFailureFailsTheSession(t *testing.T) {
	h := newHarness(t, &fakeAllocator{fail: true}, &fakeSink{})

	status, body := h.do(http.MethodPost, "/v1/session/sessions", map[string]any{
		"match_id": "m_alloc_fail",
		"players":  []string{"p_1", "p_2"},
	}, h.auth)
	h.expect(status, http.StatusServiceUnavailable, "create with a dead allocator")
	if code, _ := nested(t, body, "error", "code").(string); code != "unavailable" {
		t.Fatalf("error code = %q, want unavailable", code)
	}

	// The session exists and is failed, so a client polling it sees the truth.
	status, body = h.do(http.MethodGet, "/v1/session/sessions/s_1", nil, "dev-player-p_1")
	h.expect(status, http.StatusOK, "polling a failed session")
	if state, _ := nested(t, body, "session", "state").(string); state != StateFailed {
		t.Fatalf("session state = %q, want failed", state)
	}

	// And it cannot be resurrected by a late ready call.
	status, _ = h.do(http.MethodPost, "/v1/session/sessions/s_1/ready", map[string]any{}, h.auth)
	h.expect(status, http.StatusPreconditionFailed, "ready after failure")
}

// TestFanOutFailureKeepsTheSessionOpen pins the other half of spec section 6:
// when the fan-out fails, the report was not applied, the session stays open and
// the same result_id can be retried once the bus recovers.
func TestFanOutFailureKeepsTheSessionOpen(t *testing.T) {
	sink := &fakeSink{fail: true}
	h := newHarness(t, &fakeAllocator{}, sink)

	id := h.createSession("p_1", "p_2")
	h.ready(id)

	report := h.report(id)
	status, _ := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", report, h.auth)
	h.expect(status, http.StatusServiceUnavailable, "report while the fan-out is down")

	status, body := h.do(http.MethodGet, "/v1/session/sessions/"+id, nil, "dev-player-p_1")
	h.expect(status, http.StatusOK, "polling an unfinished session")
	if state, _ := nested(t, body, "session", "state").(string); state != StateReady {
		t.Fatalf("session state = %q, want ready (fan-out failed, report not applied)", state)
	}
	// result_id is omitempty: after a failed fan-out the key must be absent (or
	// empty), because no report was accepted.
	sessionBody, ok := nested(t, body, "session").(map[string]any)
	if !ok {
		t.Fatal("session body is not an object")
	}
	if resultID, present := sessionBody["result_id"]; present && resultID != "" {
		t.Fatalf("result_id = %v, want empty after a failed fan-out", resultID)
	}

	// The bus recovers; the retry must converge without paying twice.
	sink.fail = false
	status, body = h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", report, h.auth)
	h.expect(status, http.StatusOK, "retry after the fan-out recovers")
	if applied, _ := nested(t, body, "applied").(bool); !applied {
		t.Fatal("retried report should apply")
	}
	if state, _ := nested(t, body, "session", "state").(string); state != StateFinished {
		t.Fatalf("session state = %q, want finished", state)
	}
	if sink.calls != 2 {
		t.Fatalf("sink calls = %d, want 2 (failed run + retry)", sink.calls)
	}
}

// TestResultMustCoverTheSessionExactly pins the invariant that a report cannot
// invent or omit players: the set must equal the session roster.
func TestResultMustCoverTheSessionExactly(t *testing.T) {
	h := newHarness(t, &fakeAllocator{}, &fakeSink{})
	id := h.createSession("p_1", "p_2")
	h.ready(id)

	cases := map[string]any{
		"missing a player": map[string]any{
			"result_id": "result-short",
			"players":   []map[string]any{{"player_id": "p_1", "won": true}},
		},
		"an extra player": map[string]any{
			"result_id": "result-long",
			"players": []map[string]any{
				{"player_id": "p_1", "won": true},
				{"player_id": "p_2", "won": false},
				{"player_id": "p_3", "won": false},
			},
		},
	}
	for name, body := range cases {
		status, resp := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", body, h.auth)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, status)
		}
		if code, _ := nested(t, resp, "error", "code").(string); code != "invalid_argument" {
			t.Fatalf("%s: error code = %q, want invalid_argument", name, code)
		}
	}

	// Bad reports never move the state machine.
	_, body := h.do(http.MethodGet, "/v1/session/sessions/"+id, nil, "dev-player-p_1")
	if state, _ := nested(t, body, "session", "state").(string); state != StateReady {
		t.Fatalf("session state = %q, want ready", state)
	}
}

// TestSessionGetIsScopedToItsPlayers pins the read path: a session is not a
// public object, and a player outside the roster cannot watch it.
func TestSessionGetIsScopedToItsPlayers(t *testing.T) {
	h := newHarness(t, &fakeAllocator{}, &fakeSink{})
	id := h.createSession("p_1", "p_2")

	status, _ := h.do(http.MethodGet, "/v1/session/sessions/"+id, nil, "")
	h.expect(status, http.StatusUnauthorized, "anonymous session read")

	status, body := h.do(http.MethodGet, "/v1/session/sessions/"+id, nil, "dev-player-p_9")
	h.expect(status, http.StatusForbidden, "reading a session you are not in")
	if code, _ := nested(t, body, "error", "code").(string); code != "permission_denied" {
		t.Fatalf("error code = %q, want permission_denied", code)
	}

	status, _ = h.do(http.MethodGet, "/v1/session/sessions/s_missing", nil, "dev-player-p_1")
	h.expect(status, http.StatusNotFound, "reading an unknown session")
}

// TestCreateValidatesTheRoster keeps the session service from accepting a
// roster that no allocator could ever satisfy.
func TestCreateValidatesTheRoster(t *testing.T) {
	h := newHarness(t, &fakeAllocator{}, &fakeSink{})
	for name, players := range map[string][]string{
		"single player": {"p_1"},
		"duplicate ids": {"p_1", "p_1"},
		"empty roster":  {},
	} {
		status, body := h.do(http.MethodPost, "/v1/session/sessions", map[string]any{
			"match_id": "m_bad",
			"players":  players,
		}, h.auth)
		if status != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", name, status)
		}
		if code, _ := nested(t, body, "error", "code").(string); code != "invalid_argument" {
			t.Fatalf("%s: error code = %q, want invalid_argument", name, code)
		}
	}
}
