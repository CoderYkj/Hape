package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(url string) *Client {
	return &Client{
		BaseURL:      url,
		ServiceToken: "service-token",
		RuntimeToken: "session-token",
		Attempts:     4,
		Backoff:      time.Millisecond,
	}
}

func TestReportSendsBothCredentials(t *testing.T) {
	var gotAuth, gotSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotSession = r.Header.Get("X-Session-Token")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"session":{}}`))
	}))
	defer srv.Close()

	if err := testClient(srv.URL).ReportReady(context.Background(), "s_1", "127.0.0.1", 7777); err != nil {
		t.Fatalf("ReportReady: %v", err)
	}
	if gotAuth != "Bearer service-token" {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotSession != "session-token" {
		t.Fatalf("X-Session-Token = %q", gotSession)
	}
}

func TestSubmitResultRetriesServiceUnavailable(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			// 503 means "the fan-out failed, retry with the same result_id".
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"applied":true}`))
	}))
	defer srv.Close()

	applied, err := testClient(srv.URL).SubmitResult(context.Background(), "s_1", Report{ResultID: "r_1"})
	if err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if !applied {
		t.Fatal("expected applied=true")
	}
	if calls.Load() != 2 {
		t.Fatalf("expected exactly one retry, got %d calls", calls.Load())
	}
}

func TestSubmitResultRetriesPrecondition(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusPreconditionFailed)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"applied":true}`))
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).SubmitResult(context.Background(), "s_1", Report{ResultID: "r_1"}); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("412 must be retried once, got %d calls", calls.Load())
	}
}

func TestSubmitResultDoesNotRetryConflict(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// 409 means another result is on file: retrying cannot fix that, and the DS
		// has a bug worth surfacing.
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"conflict"}}`))
	}))
	defer srv.Close()

	if _, err := testClient(srv.URL).SubmitResult(context.Background(), "s_1", Report{ResultID: "r_1"}); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("409 must not be retried, got %d calls", calls.Load())
	}
}

func TestSubmitResultTreatsAReplayAsSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"applied":false}`))
	}))
	defer srv.Close()

	applied, err := testClient(srv.URL).SubmitResult(context.Background(), "s_1", Report{ResultID: "r_1"})
	if err != nil {
		t.Fatalf("a replay is not an error: %v", err)
	}
	if applied {
		t.Fatal("expected applied=false for a replay")
	}
}

func TestReportReadyDoesNotRetryPrecondition(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusPreconditionFailed)
	}))
	defer srv.Close()

	if err := testClient(srv.URL).ReportReady(context.Background(), "s_1", "127.0.0.1", 7777); err == nil {
		t.Fatal("expected an error")
	}
	if calls.Load() != 1 {
		t.Fatalf("ready on a finished session must not be retried, got %d calls", calls.Load())
	}
}

func TestSubmitResultSendsTheReportBody(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"applied":true}`))
	}))
	defer srv.Close()

	report := BuildReport(Config{Players: []string{"p1", "p2"}, Duration: 2 * time.Second}, "r_9")
	if _, err := testClient(srv.URL).SubmitResult(context.Background(), "s_1", report); err != nil {
		t.Fatalf("SubmitResult: %v", err)
	}
	if body["result_id"] != "r_9" {
		t.Fatalf("result_id = %v", body["result_id"])
	}
	if body["duration_seconds"].(float64) != 2 {
		t.Fatalf("duration_seconds = %v", body["duration_seconds"])
	}
	players, _ := body["players"].([]any)
	if len(players) != 2 {
		t.Fatalf("players = %v", body["players"])
	}
	first, _ := players[0].(map[string]any)
	if first["player_id"] != "p1" || first["won"] != true {
		t.Fatalf("first player = %v", first)
	}
}

func TestRetryGivesUpAfterTheLastAttempt(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := testClient(srv.URL).SubmitResult(context.Background(), "s_1", Report{ResultID: "r_1"})
	if err == nil || !strings.Contains(err.Error(), "giving up") {
		t.Fatalf("expected a giving-up error, got %v", err)
	}
	if calls.Load() != 4 {
		t.Fatalf("expected 4 attempts, got %d", calls.Load())
	}
}
