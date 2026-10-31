package matchmaking_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
	"gamebackend/services/matchmaking"
)

// TestStoresAgree pins the queue semantics: one waiting ticket per player, the
// MMR band on pop, and "a one-player match is worse than waiting".
func TestStoresAgree(t *testing.T) {
	dsn := os.Getenv("BACKEND_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BACKEND_TEST_DATABASE_URL to run the PostgreSQL/memory equality test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	queue := "equiv_q_" + suffix
	players := []string{"p_mm1_" + suffix, "p_mm2_" + suffix}
	for _, id := range players {
		if _, err := pool.Exec(ctx,
			`INSERT INTO players (id, external_id, display_name) VALUES ($1, $2, $1) ON CONFLICT DO NOTHING`,
			id, "ext-"+id); err != nil {
			t.Fatalf("seed player: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM matchmaking_tickets WHERE queue = $1`, queue)
		for _, id := range players {
			_, _ = pool.Exec(context.Background(), `DELETE FROM players WHERE id = $1`, id)
		}
	})

	for _, tc := range []struct {
		name  string
		store matchmaking.Store
	}{
		{"memory", matchmaking.NewMemory()},
		{"postgres", matchmaking.NewPG(pool)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first, err := tc.store.Create(matchmaking.Ticket{PlayerID: players[0], Queue: queue, MMR: 1000})
			if err != nil {
				t.Fatalf("enqueue: %v", err)
			}
			if first.Status != matchmaking.StatusWaiting {
				t.Fatalf("status = %q, want waiting", first.Status)
			}

			// A second waiting ticket for the same player is refused, not merged.
			if _, err := tc.store.Create(matchmaking.Ticket{PlayerID: players[0], Queue: queue, MMR: 1000}); err == nil {
				t.Fatal("second waiting ticket should be refused")
			}

			if found, ok := tc.store.FindWaitingByPlayer(queue, players[0]); !ok || found.ID != first.ID {
				t.Fatalf("FindWaitingByPlayer = (%v, %v), want the ticket just created", found.ID, ok)
			}
			if tc.store.Waiting(queue) != 1 {
				t.Fatalf("waiting = %d, want 1", tc.store.Waiting(queue))
			}

			// One player is not a match.
			if _, err := tc.store.Pop(queue, 2, 200, "m_solo"); err == nil {
				t.Fatal("popping a single waiting ticket must not form a match")
			}

			if _, err := tc.store.Create(matchmaking.Ticket{PlayerID: players[1], Queue: queue, MMR: 1050}); err != nil {
				t.Fatalf("second player enqueue: %v", err)
			}
			popped, err := tc.store.Pop(queue, 2, 200, "m_pair")
			if err != nil {
				t.Fatalf("pop: %v", err)
			}
			if len(popped) != 2 {
				t.Fatalf("popped %d tickets, want 2", len(popped))
			}
			for _, ticket := range popped {
				if ticket.Status != matchmaking.StatusMatched || ticket.MatchID != "m_pair" {
					t.Fatalf("ticket not marked matched: %+v", ticket)
				}
			}

			if tc.store.Waiting(queue) != 0 {
				t.Fatalf("waiting after pop = %d, want 0", tc.store.Waiting(queue))
			}
			if _, err := tc.store.Pop(queue, 2, 200, "m_empty"); err == nil {
				t.Fatal("popping an empty queue must fail")
			}
			if _, err := tc.store.Get("q_missing"); apierr.As(err).Code != apierr.CodeNotFound {
				t.Fatalf("unknown ticket: code = %q, want not_found", apierr.As(err).Code)
			}
		})
	}
}
