package main

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// TestCallerReloginsOnceOn401 is the assertion spec 0011 asked for and nobody had: a player
// token that expires mid-run must not end the run. The client re-authenticates once, replays
// the request with the new token, and carries on.
func TestCallerReloginsOnceOn401(t *testing.T) {
	var calls, relogins int32
	var lastAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		lastAuth = r.Header.Get("Authorization")
		if lastAuth == "Bearer stale-token" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"code":"unauthenticated","message":"token expired"}}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"player":{"id":"p_1"}}`))
	}))
	defer server.Close()

	c := &caller{
		baseURL: server.URL,
		token:   "stale-token",
		relogin: func() (string, error) {
			atomic.AddInt32(&relogins, 1)
			return "fresh-token", nil
		},
	}

	status, body, err := c.do(http.MethodGet, "/v1/account/players/p_1", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 after re-login (%s)", status, body)
	}
	if relogins != 1 {
		t.Fatalf("re-login ran %d times, want exactly 1", relogins)
	}
	if calls != 2 {
		t.Fatalf("server saw %d calls, want 2 (401 then the replay)", calls)
	}
	if lastAuth != "Bearer fresh-token" {
		t.Fatalf("replay used %q, want the renewed token", lastAuth)
	}
	if c.token != "fresh-token" {
		t.Fatalf("caller kept %q instead of the renewed token", c.token)
	}
}

// TestCallerDoesNotLoopOnPersistent401: one replay, then the 401 is returned. A client that
// retried forever would turn a broken deployment into an infinite login storm.
func TestCallerDoesNotLoopOnPersistent401(t *testing.T) {
	var relogins, calls int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	c := &caller{
		baseURL: server.URL,
		token:   "always-rejected",
		relogin: func() (string, error) {
			atomic.AddInt32(&relogins, 1)
			return "still-rejected", nil
		},
	}
	status, _, err := c.do(http.MethodGet, "/v1/account/players/p_1", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
	if relogins != 1 {
		t.Fatalf("re-login ran %d times, want 1", relogins)
	}
	if calls != 2 {
		t.Fatalf("server saw %d calls, want 2", calls)
	}
}

// TestCallerWithoutReloginSurfacesThe401: a token that cannot be renewed (a scripted run)
// must report the 401 rather than invent a retry.
func TestCallerWithoutReloginSurfacesThe401(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	c := &caller{baseURL: server.URL, token: "only-token"}
	status, _, err := c.do(http.MethodGet, "/v1/account/players/p_1", nil)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", status)
	}
}
