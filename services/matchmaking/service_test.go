package matchmaking

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

	"gamebackend/pkg/httpx"
)

// brokenStore fails the two lookups the service must not paper over.
type brokenStore struct {
	Store
	failLookup bool
	failCreate bool
}

func (b brokenStore) FindWaitingByPlayer(context.Context, string, string) (Ticket, bool, error) {
	if b.failLookup {
		return Ticket{}, false, errors.New("database is down")
	}
	return b.Store.FindWaitingByPlayer(context.Background(), "", "")
}

func newService(t *testing.T, store Store, ratings RatingLookup) *httptest.Server {
	t.Helper()
	router := httpx.NewRouter("matchmaking", slog.New(slog.NewTextHandler(io.Discard, nil)),
		httpx.NewDevAuth("dev-service"))
	NewWithStore(slog.New(slog.NewTextHandler(io.Discard, nil)), store, ratings).Register(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func enqueue(t *testing.T, server *httptest.Server, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/matchmaking/tickets", bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer dev-player-p_1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

func errorCode(t *testing.T, body map[string]any) string {
	t.Helper()
	errObj, ok := body["error"].(map[string]any)
	if !ok {
		t.Fatalf("no error envelope in %v", body)
	}
	code, _ := errObj["code"].(string)
	return code
}

// TestEnqueueFailsWhenTheRatingLookupFails pins the difference between "we could
// not read your MMR" and "you have no MMR": guessing 1000 would silently poison
// match quality.
func TestEnqueueFailsWhenTheRatingLookupFails(t *testing.T) {
	failing := func(context.Context, string) (int64, error) {
		return 0, errors.New("rank service unavailable")
	}
	server := newService(t, NewMemory(), failing)

	status, body := enqueue(t, server, map[string]any{"queue": "ranked_1v1"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if code := errorCode(t, body); code != "unavailable" {
		t.Fatalf("code = %q, want unavailable", code)
	}
}

// TestEnqueueFailsWhenTheQueueLookupFails: the ticket must not be created twice
// because the first lookup errored.
func TestEnqueueFailsWhenTheQueueLookupFails(t *testing.T) {
	server := newService(t, brokenStore{Store: NewMemory(), failLookup: true},
		func(context.Context, string) (int64, error) { return 1000, nil })

	status, body := enqueue(t, server, map[string]any{"queue": "ranked_1v1"})
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if code := errorCode(t, body); code != "unavailable" {
		t.Fatalf("code = %q, want unavailable", code)
	}
}

// TestEnqueueStillWorksWhenEverythingIsHealthy keeps the failure tests honest.
func TestEnqueueStillWorksWhenEverythingIsHealthy(t *testing.T) {
	server := newService(t, NewMemory(), func(context.Context, string) (int64, error) { return 1000, nil })

	status, body := enqueue(t, server, map[string]any{"queue": "ranked_1v1"})
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%v)", status, body)
	}
	ticket, ok := body["ticket"].(map[string]any)
	if !ok {
		t.Fatalf("no ticket in %v", body)
	}
	if mmr, _ := ticket["mmr"].(float64); int64(mmr) != 1000 {
		t.Fatalf("mmr = %v, want 1000", ticket["mmr"])
	}
}
