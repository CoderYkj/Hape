package rank_test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"gamebackend/pkg/httpx"
	"gamebackend/services/rank"
)

// newServer wires the rank service the way platform does, with a dev player token, so the
// handler is exercised through the real router (auth, decoding, error rendering included).
func newServer(t *testing.T) (string, rank.Store) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := rank.NewMemory()
	router := httpx.NewRouter("rank", log, httpx.NewDevAuth("dev-service"))
	rank.NewWithStore(log, store).Register(router)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server.URL, store
}

func seed(t *testing.T, store rank.Store, players int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < players; i++ {
		player := "p_" + strconv.Itoa(i)
		// Wins push the rating up, so the ordering is deterministic and not just insertion.
		for w := 0; w <= i; w++ {
			if _, _, _, err := store.Apply(ctx, rank.DefaultBoard, player,
				player+"-win-"+strconv.Itoa(w), true); err != nil {
				t.Fatalf("seed %s: %v", player, err)
			}
		}
	}
}

// get asks for a leaderboard page and returns the status plus the entry count.
func get(t *testing.T, base, query string) (int, int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/v1/rank/leaderboards/"+rank.DefaultBoard+query, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer dev-player-p_0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)

	var parsed struct {
		Leaderboard struct {
			Entries []rank.Entry `json:"entries"`
		} `json:"leaderboard"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	return resp.StatusCode, len(parsed.Leaderboard.Entries), parsed.Error.Code
}

// TestLeaderboardLimitIsHonouredAndClamped: the page size used to be a hardcoded 50 with no
// way to ask for anything else. It is a query parameter now - bounded, not rejected when
// oversized, and validated when it is not a number.
func TestLeaderboardLimitIsHonouredAndClamped(t *testing.T) {
	base, store := newServer(t)
	seed(t, store, 5)

	if status, n, _ := get(t, base, ""); status != http.StatusOK || n != 5 {
		t.Fatalf("default: status=%d entries=%d, want 200/5", status, n)
	}
	if status, n, _ := get(t, base, "?limit=2"); status != http.StatusOK || n != 2 {
		t.Fatalf("limit=2: status=%d entries=%d, want 200/2", status, n)
	}
	// Above the cap is clamped, not refused: a dashboard asking for too much gets a full page.
	if status, n, _ := get(t, base, "?limit=9999"); status != http.StatusOK || n != 5 {
		t.Fatalf("limit=9999: status=%d entries=%d, want 200/5 (clamped, all seeded)", status, n)
	}
	for _, bad := range []string{"?limit=abc", "?limit=0", "?limit=-3"} {
		status, _, code := get(t, base, bad)
		if status != http.StatusBadRequest || code != "invalid_argument" {
			t.Fatalf("%s: status=%d code=%q, want 400/invalid_argument", bad, status, code)
		}
	}
}
