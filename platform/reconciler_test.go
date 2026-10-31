package platform_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
