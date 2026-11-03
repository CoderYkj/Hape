package platform_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gamebackend/pkg/battlealloc"
	"gamebackend/platform"
)

func newAppWith(t *testing.T, cfg platform.Config) *platform.Platform {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	app, err := platform.New(cfg)
	if err != nil {
		t.Fatalf("platform.New: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

// TestReconcilerReturnsImmediatelyWhenDisabled is why cmd/platform can start it
// unconditionally: the default configuration must not leak a goroutine.
func TestReconcilerReturnsImmediatelyWhenDisabled(t *testing.T) {
	app := newAppWith(t, platform.Config{Env: "test", DevAuth: true, Allocator: "local"})
	if got := app.ReconcileInterval(); got != 0 {
		t.Fatalf("ReconcileInterval = %s, want 0 by default", got)
	}

	done := make(chan struct{})
	go func() {
		app.RunReconciler(context.Background())
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunReconciler did not return with the sweep disabled")
	}
}

// TestReconcilerFailsStaleSessions exercises the loop end to end: a session that
// never reports ready is swept into failed without any HTTP call from a client.
func TestReconcilerFailsStaleSessions(t *testing.T) {
	app := newAppWith(t, platform.Config{
		Env:               "test",
		DevAuth:           true,
		Allocator:         "local",
		ReconcileInterval: 10 * time.Millisecond,
		SessionStaleAfter: time.Millisecond, // every session is stale almost immediately
	})
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go app.RunReconciler(ctx)

	sessionID := createSession(t, server.URL)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if state := sessionState(t, server.URL, sessionID); state == "failed" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("session %s was never swept; last state = %q", sessionID, sessionState(t, server.URL, sessionID))
}

func createSession(t *testing.T, baseURL string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"match_id": "m_reconcile",
		"players":  []string{"p_1", "p_2"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/session/sessions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dev-service")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode create response (status %d): %v", resp.StatusCode, err)
	}
	if decoded.Session.ID == "" {
		t.Fatalf("create session: empty id (status %d)", resp.StatusCode)
	}
	return decoded.Session.ID
}

func sessionState(t *testing.T, baseURL, sessionID string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/session/sessions/"+sessionID, nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer dev-player-p_1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded struct {
		Session struct {
			State string `json:"state"`
		} `json:"session"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode session (status %d): %v", resp.StatusCode, err)
	}
	return decoded.Session.State
}

// stubAllocator scripts the one thing a test cannot ask a vendor for: a provider that
// hands out servers and then refuses to let go.
type stubAllocator struct {
	failRelease bool
}

func (s *stubAllocator) Provider() string { return "stub" }

func (s *stubAllocator) Allocate(_ context.Context, req battlealloc.Request) (battlealloc.Allocation, error) {
	return battlealloc.Allocation{
		ID:       "alloc-" + req.SessionID,
		Provider: "stub",
		Address:  "127.0.0.1",
		Port:     7777,
	}, nil
}

func (s *stubAllocator) Release(context.Context, string) error {
	if s.failRelease {
		return errors.New("provider still running")
	}
	return nil
}

// TestReconcileOncePublishesTheReleaseBacklog is the end of the billing story: a release
// that fails is retried, and the backlog becomes numbers an alert can watch. Without the
// gauges, "we cannot confirm some servers stopped" only exists in a log nobody pages on.
func TestReconcileOncePublishesTheReleaseBacklog(t *testing.T) {
	alloc := &stubAllocator{failRelease: true}
	app := newAppWith(t, platform.Config{
		Env:               "test",
		DevAuth:           true,
		Allocator:         "local",
		AllocatorOverride: alloc,
	})
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)

	sessionID, runtimeToken := createSessionWithToken(t, server.URL, "m_metrics")
	markReady(t, server.URL, sessionID, runtimeToken)
	reportResult(t, server.URL, sessionID, runtimeToken)

	if err := app.ReconcileOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}

	body := scrapeMetrics(t, server.URL)
	for _, want := range []string{
		// One session is stuck; it failed once at report time and once on the sweep.
		`backend_release_pending_sessions{service="platform"} 1`,
		`backend_release_max_attempts{service="platform"} 2`,
		`backend_release_failed_total{service="platform"} 1`,
		`backend_release_confirmed_total{service="platform"} 0`,
		`backend_release_oldest_pending_seconds{service="platform"}`,
		// The ledger's "this can still be billing" number: the release failed, so the
		// allocation has no confirmed release.
		`backend_unreleased_allocations{service="platform"} 1`,
		`backend_unreleased_oldest_seconds{service="platform"}`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics are missing %q:\n%s", want, body)
		}
	}

	// The provider recovers: the next sweep clears the backlog, and the gauge must drop
	// to zero rather than staying at its high-water mark.
	alloc.failRelease = false
	if err := app.ReconcileOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("ReconcileOnce (recovered): %v", err)
	}
	body = scrapeMetrics(t, server.URL)
	if !strings.Contains(body, `backend_release_pending_sessions{service="platform"} 0`) {
		t.Fatalf("the backlog gauge did not clear:\n%s", body)
	}
	if !strings.Contains(body, `backend_release_confirmed_total{service="platform"} 1`) {
		t.Fatalf("the confirmed counter did not move:\n%s", body)
	}
	if !strings.Contains(body, `backend_unreleased_allocations{service="platform"} 0`) {
		t.Fatalf("the unreleased gauge did not clear:\n%s", body)
	}
}

func createSessionWithToken(t *testing.T, baseURL, matchID string) (sessionID, runtimeToken string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"match_id": matchID,
		"players":  []string{"p_1", "p_2"},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/session/sessions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dev-service")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
		RuntimeToken string `json:"runtime_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode create response (status %d): %v", resp.StatusCode, err)
	}
	if decoded.Session.ID == "" || decoded.RuntimeToken == "" {
		t.Fatalf("create session: empty id or token (status %d)", resp.StatusCode)
	}
	return decoded.Session.ID, decoded.RuntimeToken
}

func markReady(t *testing.T, baseURL, sessionID, runtimeToken string) {
	t.Helper()
	postAs(t, baseURL, "/v1/session/sessions/"+sessionID+"/ready", runtimeToken, map[string]any{"port": 7777})
}

func reportResult(t *testing.T, baseURL, sessionID, runtimeToken string) {
	t.Helper()
	postAs(t, baseURL, "/v1/session/sessions/"+sessionID+"/results", runtimeToken, map[string]any{
		"result_id":        "res-" + sessionID,
		"duration_seconds": 60,
		"players": []map[string]any{
			{"player_id": "p_1", "won": true, "score": 500},
			{"player_id": "p_2", "won": false, "score": 100},
		},
	})
}

func postAs(t *testing.T, baseURL, path, runtimeToken string, payload map[string]any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dev-service")
	req.Header.Set("X-Session-Token", runtimeToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post %s: %v", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("post %s: status %d: %s", path, resp.StatusCode, body)
	}
}

func scrapeMetrics(t *testing.T, baseURL string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("get metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read metrics: %v", err)
	}
	return string(body)
}

// TestAlertRulesWatchMetricsWePublish is the drift guard for the rules file: every metric a
// rule references must actually be exposed, otherwise the alert looks like coverage and can
// never fire (spec 0019).
func TestAlertRulesWatchMetricsWePublish(t *testing.T) {
	raw, err := os.ReadFile("../deploy/prometheus/hape.rules.yml")
	if err != nil {
		t.Fatalf("read rules: %v", err)
	}
	references := regexp.MustCompile(`backend_[a-z_]+`).FindAllString(string(raw), -1)
	if len(references) == 0 {
		t.Fatal("the rules file references no metrics")
	}

	app := newAppWith(t, platform.Config{Env: "test", DevAuth: true, Allocator: "local"})
	server := httptest.NewServer(app.Handler())
	t.Cleanup(server.Close)
	if err := app.ReconcileOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("ReconcileOnce: %v", err)
	}
	// The request counter only exists once something has been served, so serve one op: the
	// rules reference it and the drift guard has to see it.
	doJSON(t, server.URL, http.MethodPost, "/v1/account/sessions",
		map[string]any{"external_id": "rules-drift"}, "dev-service")
	body := scrapeMetrics(t, server.URL)

	checked := map[string]bool{}
	for _, name := range references {
		if checked[name] {
			continue
		}
		checked[name] = true
		// Every exposed metric carries a service label, so the rendered series is `<name>{`.
		if !strings.Contains(body, name+"{") {
			t.Errorf("the rules reference %s but nothing exports it", name)
		}
	}
	t.Logf("checked %d distinct metrics referenced by the rules", len(checked))
}
