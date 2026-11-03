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
	"time"

	"gamebackend/pkg/battlealloc"
	"gamebackend/pkg/httpx"
)

const serviceToken = "dev-service"

// fakeAllocator fails on demand, so "the provider is out of capacity" is a
// one-line switch instead of a vendor account.
type fakeAllocator struct {
	fail     bool
	released []string
	// onAllocate runs inside Allocate, before it returns: the test uses it to close the
	// session while the "provider" is still working, which is what a slow allocation plus a
	// stale sweep looks like from the handler's side.
	onAllocate func()
	// releaseErr makes the next release fail, which is how "the provider did not let go"
	// is scripted. Nil means a release succeeds.
	releaseErr error
}

func (f *fakeAllocator) Provider() string { return "fake" }

func (f *fakeAllocator) Allocate(context.Context, battlealloc.Request) (battlealloc.Allocation, error) {
	if f.onAllocate != nil {
		f.onAllocate()
	}
	if f.fail {
		return battlealloc.Allocation{}, errors.New("provider quota exceeded")
	}
	return battlealloc.Allocation{ID: "alloc-1", Provider: "fake", Address: "10.0.0.9", Port: 7777}, nil
}

func (f *fakeAllocator) Release(_ context.Context, allocationID string) error {
	if allocationID == "" {
		return errors.New("release called without an allocation id")
	}
	f.released = append(f.released, allocationID)
	return f.releaseErr
}

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
	t     *testing.T
	srv   *httptest.Server
	app   *Service
	store Store
	auth  string
	// token is the per-session DS credential of the most recent createSession call.
	token string
}

func newHarness(t *testing.T, alloc battlealloc.Allocator, sink ResultSink) *harness {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	router := httpx.NewRouter("session", log, httpx.NewDevAuth(serviceToken))
	store := NewMemory()
	app := NewWithStore(log, store, alloc, sink)
	app.Register(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return &harness{t: t, srv: server, app: app, store: store, auth: serviceToken}
}

// do issues one request and returns the status plus the decoded body (nil for
// non-JSON responses, which this package never produces).
func (h *harness) do(method, path string, body any, token string, headers ...string) (int, map[string]any) {
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
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
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
	token, _ := body["runtime_token"].(string)
	if token == "" {
		h.t.Fatal("session create: no runtime_token in response")
	}
	h.token = token
	return id
}

func (h *harness) ready(id string) {
	h.t.Helper()
	status, _ := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/ready", map[string]any{}, h.auth, "X-Session-Token", h.token)
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
	// Its id is generated, so it is read from the in-memory store rather than
	// assumed: with a persistent store a fixed id would also collide across runs.
	memory, ok := h.store.(*Memory)
	if !ok {
		t.Fatalf("harness store is %T, want *Memory", h.store)
	}
	var sessionID string
	for id := range memory.byID {
		sessionID = id
	}
	if sessionID == "" {
		t.Fatal("the failed session was not recorded")
	}

	status, body = h.do(http.MethodGet, "/v1/session/sessions/"+sessionID, nil, "dev-player-p_1")
	h.expect(status, http.StatusOK, "polling a failed session")
	if state, _ := nested(t, body, "session", "state").(string); state != StateFailed {
		t.Fatalf("session state = %q, want failed", state)
	}

	// And it cannot be resurrected by a late ready call. The caller has no session
	// token (the allocation never produced an instance, so nobody was given one), so
	// the refusal is 401 - the credential check runs before the state machine, which
	// keeps unauthenticated callers from learning anything about the session's state.
	status, _ = h.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/ready", map[string]any{}, h.auth)
	h.expect(status, http.StatusUnauthorized, "ready after failure without the session token")

	// A session whose allocation failed still refuses a *valid* token on state
	// grounds; that is the transition check, reached only with the credential.
	memoryToken := memory.byID[sessionID].RuntimeToken
	status, _ = h.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/ready", map[string]any{}, h.auth, "X-Session-Token", memoryToken)
	h.expect(status, http.StatusPreconditionFailed, "ready after failure with the session token")
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
	status, _ := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", report, h.auth, "X-Session-Token", h.token)
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
	status, body = h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", report, h.auth, "X-Session-Token", h.token)
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
		status, resp := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", body, h.auth, "X-Session-Token", h.token)
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

// TestSubmitResultReleasesTheServer closes the "allocated but never released"
// leak: a finished match hands the dedicated server back exactly once.
func TestSubmitResultReleasesTheServer(t *testing.T) {
	alloc := &fakeAllocator{}
	h := newHarness(t, alloc, &fakeSink{})
	id := h.createSession("p_1", "p_2")
	h.ready(id)

	status, _ := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", h.report(id), h.auth, "X-Session-Token", h.token)
	h.expect(status, http.StatusOK, "battle report")

	if len(alloc.released) != 1 {
		t.Fatalf("releases = %v, want exactly one", alloc.released)
	}
	if alloc.released[0] != "alloc-1" {
		t.Fatalf("released %q, want the allocation id", alloc.released[0])
	}

	// A replay must not release twice: the server is already gone.
	status, _ = h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", h.report(id), h.auth, "X-Session-Token", h.token)
	h.expect(status, http.StatusOK, "replay")
	if len(alloc.released) != 1 {
		t.Fatalf("releases after replay = %v, want still one", alloc.released)
	}
}

// TestExpireStaleKillsAbandonedSessionsAndReleasesThem is the reconciler's
// contract: a session stuck in allocating is failed, and its server returned.
func TestExpireStaleKillsAbandonedSessionsAndReleasesThem(t *testing.T) {
	alloc := &fakeAllocator{}
	h := newHarness(t, alloc, &fakeSink{})
	id := h.createSession("p_1", "p_2")

	// Fresh: nothing is stale yet.
	expired, err := h.app.ExpireStale(context.Background(), time.Now(), 5*time.Minute)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("expired = %v, want nothing for a fresh session", expired)
	}

	// Ten minutes later the same session is abandoned.
	expired, err = h.app.ExpireStale(context.Background(), time.Now().Add(10*time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if len(expired) != 1 || expired[0].ID != id {
		t.Fatalf("expired = %v, want the one abandoned session %s", expired, id)
	}
	if len(alloc.released) != 1 || alloc.released[0] != "alloc-1" {
		t.Fatalf("released = %v, want the abandoned allocation", alloc.released)
	}

	// The state is visible to a player polling the session.
	_, body := h.do(http.MethodGet, "/v1/session/sessions/"+id, nil, "dev-player-p_1")
	if state, _ := nested(t, body, "session", "state").(string); state != StateFailed {
		t.Fatalf("session state = %q, want failed", state)
	}

	// And a second sweep finds nothing: expiry is not cumulative.
	expired, err = h.app.ExpireStale(context.Background(), time.Now().Add(20*time.Minute), 5*time.Minute)
	if err != nil {
		t.Fatalf("second ExpireStale: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("second sweep expired %v, want nothing", expired)
	}
}

// TestExpireStaleLeavesFinishedSessionsAlone keeps the sweep from rewriting
// history: a finished match is not stale, however old it is.
func TestExpireStaleLeavesFinishedSessionsAlone(t *testing.T) {
	alloc := &fakeAllocator{}
	h := newHarness(t, alloc, &fakeSink{})
	id := h.createSession("p_1", "p_2")
	h.ready(id)
	status, _ := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", h.report(id), h.auth, "X-Session-Token", h.token)
	h.expect(status, http.StatusOK, "battle report")

	expired, err := h.app.ExpireStale(context.Background(), time.Now().Add(24*time.Hour), time.Minute)
	if err != nil {
		t.Fatalf("ExpireStale: %v", err)
	}
	if len(expired) != 0 {
		t.Fatalf("expired = %v, want nothing for a finished session", expired)
	}
	if len(alloc.released) != 1 {
		t.Fatalf("releases = %v, want only the finish-time release", alloc.released)
	}
}

// TestExpireStaleRefusesANonsenseWindow keeps a zero window from killing every
// session the moment a timer fires.
func TestExpireStaleRefusesANonsenseWindow(t *testing.T) {
	h := newHarness(t, &fakeAllocator{}, &fakeSink{})
	if _, err := h.app.ExpireStale(context.Background(), time.Now(), 0); err == nil {
		t.Fatal("ExpireStale with a zero window should fail loudly")
	}
}

// TestSessionGetByMatchIsTheClientReadPath pins the op that closes the client loop: a
// ticket yields a match id, and this yields the address to connect to - with the same
// scoping as session.get, because it is the same object.
func TestSessionGetByMatchIsTheClientReadPath(t *testing.T) {
	h := newHarness(t, &fakeAllocator{}, &fakeSink{})
	id := h.createSession("p_1", "p_2")

	status, body := h.do(http.MethodGet, "/v1/session/sessions/by-match/m_test", nil, "dev-player-p_1")
	h.expect(status, http.StatusOK, "a player reading their own match")
	if got, _ := nested(t, body, "session", "id").(string); got != id {
		t.Fatalf("session id = %q, want %q", got, id)
	}
	// The DS credential must not be reachable from a player-facing read, whichever
	// route they take.
	sessionBody, ok := nested(t, body, "session").(map[string]any)
	if !ok {
		t.Fatal("session body is not an object")
	}
	if token, present := sessionBody["runtime_token"]; present && token != "" {
		t.Fatalf("runtime_token = %v in the player read path", token)
	}

	status, _ = h.do(http.MethodGet, "/v1/session/sessions/by-match/m_test", nil, "dev-player-p_9")
	h.expect(status, http.StatusForbidden, "reading a match you are not in")

	status, _ = h.do(http.MethodGet, "/v1/session/sessions/by-match/m_test", nil, "")
	h.expect(status, http.StatusUnauthorized, "anonymous match lookup")

	status, _ = h.do(http.MethodGet, "/v1/session/sessions/by-match/m_unknown", nil, "dev-player-p_1")
	h.expect(status, http.StatusNotFound, "a match that was never allocated")
}

// TestFailedReleaseIsRetriedByTheReconciler covers the billing path: the report is
// accepted, the server will not let go, and the failure is recorded instead of logged
// and forgotten. A successful retry clears it.
func TestFailedReleaseIsRetriedByTheReconciler(t *testing.T) {
	alloc := &fakeAllocator{releaseErr: errors.New("provider still running")}
	h := newHarness(t, alloc, &fakeSink{})

	id := h.createSession("p_1", "p_2")
	h.ready(id)

	// The report must still be accepted: a provider that is slow to release cannot be
	// allowed to fail a player's settlement.
	status, body := h.do(http.MethodPost, "/v1/session/sessions/"+id+"/results", h.report(id), h.auth, "X-Session-Token", h.token)
	h.expect(status, http.StatusOK, "report while the release is failing")
	if applied, _ := nested(t, body, "applied").(bool); !applied {
		t.Fatal("the report should have been applied")
	}

	memory, ok := h.store.(*Memory)
	if !ok {
		t.Fatalf("harness store is %T, want *Memory", h.store)
	}
	pending := memory.byID[id]
	if !pending.ReleasePending {
		t.Fatal("a failed release was not recorded as pending")
	}
	if pending.ReleaseAttempts != 1 {
		t.Fatalf("release attempts = %d, want 1", pending.ReleaseAttempts)
	}

	// The provider recovers; the sweep clears the record.
	alloc.releaseErr = nil
	result, err := h.app.RetryReleases(context.Background(), 10)
	if err != nil {
		t.Fatalf("RetryReleases: %v", err)
	}
	if result.Cleared != 1 || result.Failed != 0 {
		t.Fatalf("retry result = %+v, want 1 cleared and 0 failed", result)
	}
	if after := memory.byID[id]; after.ReleasePending {
		t.Fatal("the pending flag survived a successful retry")
	}
}

// TestFailedReleaseRetryKeepsCounting pins the counter that makes a persistent leak
// visible: the same session failing twice must read as two attempts, not one.
func TestFailedReleaseRetryKeepsCounting(t *testing.T) {
	alloc := &fakeAllocator{releaseErr: errors.New("provider still running")}
	h := newHarness(t, alloc, &fakeSink{})
	id := h.createSession("p_1", "p_2")
	h.ready(id)
	h.expectAfterReport(t, id)

	result, err := h.app.RetryReleases(context.Background(), 10)
	if err != nil {
		t.Fatalf("RetryReleases: %v", err)
	}
	if result.Failed != 1 || result.Cleared != 0 {
		t.Fatalf("retry result = %+v, want 1 failed and 0 cleared", result)
	}
	memory := h.store.(*Memory)
	if got := memory.byID[id].ReleaseAttempts; got != 2 {
		t.Fatalf("release attempts = %d, want 2 (the first failure plus one retry)", got)
	}
	if !memory.byID[id].ReleasePending {
		t.Fatal("a still-failing release must stay pending")
	}
}

// expectAfterReport posts a battle report and requires it to be accepted.
func (h *harness) expectAfterReport(t *testing.T, sessionID string) {
	t.Helper()
	status, _ := h.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/results",
		h.report(sessionID), h.auth, "X-Session-Token", h.token)
	h.expect(status, http.StatusOK, "battle report")
}

// TestReleaseStatsDescribesTheBacklog is what the gauges are built from: how many, how
// bad, and how long. A healthy deployment reports zeroes, not an error.
func TestReleaseStatsDescribesTheBacklog(t *testing.T) {
	alloc := &fakeAllocator{releaseErr: errors.New("provider still running")}
	h := newHarness(t, alloc, &fakeSink{})

	healthy, err := h.app.ReleaseStats(context.Background(), time.Now())
	if err != nil {
		t.Fatalf("ReleaseStats on an empty backlog: %v", err)
	}
	if healthy.Pending != 0 || healthy.MaxAttempts != 0 || healthy.OldestSeconds != 0 {
		t.Fatalf("empty backlog reported %+v", healthy)
	}

	id := h.createSession("p_1", "p_2")
	h.ready(id)
	h.expectAfterReport(t, id)

	stats, err := h.app.ReleaseStats(context.Background(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatalf("ReleaseStats: %v", err)
	}
	if stats.Pending != 1 {
		t.Fatalf("pending = %d, want 1", stats.Pending)
	}
	if stats.MaxAttempts != 1 {
		t.Fatalf("max attempts = %d, want 1 (one failed release)", stats.MaxAttempts)
	}
	// The clock is a minute ahead of the session, so the age must be about a minute.
	if stats.OldestSeconds < 55 || stats.OldestSeconds > 65 {
		t.Fatalf("oldest = %ds, want ~60s", stats.OldestSeconds)
	}
}

// TestAllocationRacingTheStaleSweepIsReleased pins the fix for a real race found against the
// live provider: a managed allocation can take longer than SESSION_STALE_AFTER, so the sweep
// closes the session while the handler is still waiting. The instance then has no owner.
//
// The contract: the allocation is recorded (so a failed release stays retryable), the server
// is released immediately, and the caller gets 503 instead of a "failed" session holding a
// running server.
func TestAllocationRacingTheStaleSweepIsReleased(t *testing.T) {
	alloc := &fakeAllocator{}
	h := newHarness(t, alloc, &fakeSink{})

	alloc.onAllocate = func() {
		memory, ok := h.store.(*Memory)
		if !ok {
			t.Fatalf("harness store is %T, want *Memory", h.store)
		}
		for id := range memory.byID {
			if _, err := memory.Update(context.Background(), id, func(sp *Session) error {
				sp.State = StateFailed
				return nil
			}); err != nil {
				t.Fatalf("sweeping the session mid-allocation: %v", err)
			}
		}
	}

	status, body := h.do(http.MethodPost, "/v1/session/sessions", map[string]any{
		"match_id": "m_race",
		"players":  []string{"p_1", "p_2"},
	}, h.auth)
	h.expect(status, http.StatusServiceUnavailable, "create losing the race with the sweep")
	if code, _ := nested(t, body, "error", "code").(string); code != "unavailable" {
		t.Fatalf("error code = %q, want unavailable", code)
	}

	// The instance was handed back, not abandoned.
	if len(alloc.released) != 1 {
		t.Fatalf("released %d allocations, want 1 (%v)", len(alloc.released), alloc.released)
	}

	// And the allocation stayed on the session record: that is what makes a *failed* release
	// retryable instead of lost.
	memory := h.store.(*Memory)
	for _, session := range memory.byID {
		if session.AllocationID == "" {
			continue
		}
		if session.ReleasePending {
			t.Fatal("a successful release must not leave the session pending")
		}
		return
	}
	t.Fatal("the allocation was not recorded on the session")
}

// TestRaceWithAnUnconfirmedReleaseStaysRetryable is the other half: if handing the instance
// back fails, the session must stay pending so the reconciler keeps trying.
func TestRaceWithAnUnconfirmedReleaseStaysRetryable(t *testing.T) {
	alloc := &fakeAllocator{releaseErr: errors.New("provider still running")}
	h := newHarness(t, alloc, &fakeSink{})

	alloc.onAllocate = func() {
		memory := h.store.(*Memory)
		for id := range memory.byID {
			_, _ = memory.Update(context.Background(), id, func(sp *Session) error {
				sp.State = StateFailed
				return nil
			})
		}
	}

	status, _ := h.do(http.MethodPost, "/v1/session/sessions", map[string]any{
		"match_id": "m_race_pending",
		"players":  []string{"p_1", "p_2"},
	}, h.auth)
	h.expect(status, http.StatusServiceUnavailable, "create losing the race with a failing release")

	memory := h.store.(*Memory)
	found := false
	for _, session := range memory.byID {
		if session.AllocationID == "" {
			continue
		}
		found = true
		if !session.ReleasePending {
			t.Fatal("an unconfirmed release must be recorded as pending")
		}
		if session.ReleaseAttempts != 1 {
			t.Fatalf("release attempts = %d, want 1", session.ReleaseAttempts)
		}
	}
	if !found {
		t.Fatal("the allocation was not recorded on the session")
	}
}

// TestAllocationLedgerRecordsTheTimeline is the report's happy path: one match, one server,
// allocated and released, with the duration already derived.
func TestAllocationLedgerRecordsTheTimeline(t *testing.T) {
	h := newHarness(t, &fakeAllocator{}, &fakeSink{})
	id := h.createSession("p_1", "p_2")
	h.ready(id)
	h.expectAfterReport(t, id)

	now := time.Now().UTC().Add(time.Minute)
	rows, err := h.app.Allocations(context.Background(), AllocationQuery{}, now)
	if err != nil {
		t.Fatalf("Allocations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ledger has %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row.SessionID != id || row.Provider != "fake" || row.AllocationID != "alloc-1" {
		t.Fatalf("unexpected ledger row: %+v", row)
	}
	if row.AllocatedAt.IsZero() {
		t.Fatal("allocated_at was not recorded")
	}
	if row.ReleasedAt == nil {
		t.Fatal("a successful release was not recorded")
	}
	if row.Unreleased {
		t.Fatal("a confirmed release is still flagged as unreleased")
	}
	if row.InstanceSeconds < 0 {
		t.Fatalf("instance seconds = %d", row.InstanceSeconds)
	}
}

// TestAllocationLedgerShowsWhatIsStillUnaccounted pins the number that matters for billing:
// an allocation with no confirmed release is the only thing that can still be charging.
func TestAllocationLedgerShowsWhatIsStillUnaccounted(t *testing.T) {
	alloc := &fakeAllocator{releaseErr: errors.New("provider still running")}
	h := newHarness(t, alloc, &fakeSink{})
	id := h.createSession("p_1", "p_2")
	h.ready(id)
	h.expectAfterReport(t, id)

	now := time.Now().UTC().Add(30 * time.Second)
	rows, err := h.app.Allocations(context.Background(), AllocationQuery{UnreleasedOnly: true}, now)
	if err != nil {
		t.Fatalf("Allocations: %v", err)
	}
	if len(rows) != 1 || rows[0].SessionID != id {
		t.Fatalf("unreleased view = %+v, want the one session %s", rows, id)
	}
	row := rows[0]
	if !row.Unreleased || row.ReleasedAt != nil {
		t.Fatalf("expected an unreleased row, got %+v", row)
	}
	if !row.ReleasePending || row.ReleaseAttempts != 1 {
		t.Fatalf("pending=%t attempts=%d, want true/1", row.ReleasePending, row.ReleaseAttempts)
	}
	// The clock is 30s past the allocation, so the age is about 30s (a second of slack for
	// the allocation timestamp being taken slightly before `now` was computed).
	if row.InstanceSeconds < 29 || row.InstanceSeconds > 31 {
		t.Fatalf("instance seconds = %d, want ~30", row.InstanceSeconds)
	}
}
