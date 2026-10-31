package platform

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strconv"
	"testing"
	"time"

	"gamebackend/pkg/pg"
	"gamebackend/services/session"
)

// TestSettlementIsAtomic is the acceptance test of ADR 0007: if any domain write
// fails, none of them land. Before the single transaction, a partial fan-out left
// one player paid and the other not, while the session stayed open.
func TestSettlementIsAtomic(t *testing.T) {
	dsn := os.Getenv("BACKEND_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BACKEND_TEST_DATABASE_URL to run the settlement atomicity test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	player := "p_atomic_" + suffix
	if _, err := pool.Exec(ctx,
		`INSERT INTO players (id, external_id, display_name) VALUES ($1, $2, $1)`,
		player, "ext-"+player); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM progress_grants WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM player_progress WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM wallets WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM rating_events WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM player_ratings WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM players WHERE id = $1`, player)
	})

	sink := &matchSink{
		log:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		pool: pool,
	}

	// A valid player first, then a ghost: the first player's writes must be rolled
	// back when the second one fails.
	ghost := "p_ghost_" + suffix
	match := session.FinishedMatch{
		SessionID:       "s_atomic_" + suffix,
		MatchID:         "m_atomic_" + suffix,
		ResultID:        "res_atomic_" + suffix,
		DurationSeconds: 300,
		Players: []session.PlayerResult{
			{PlayerID: player, Won: true, Score: 900, Kills: 9, Deaths: 2, Damage: 1800},
			{PlayerID: ghost, Won: false, Score: 400, Kills: 2, Deaths: 9, Damage: 700},
		},
	}

	if err := sink.MatchFinished(ctx, match); err == nil {
		t.Fatal("fan-out with a ghost player should fail")
	}
	for name, query := range map[string]string{
		"player_progress": `SELECT count(*) FROM player_progress WHERE player_id = $1`,
		"progress_grants": `SELECT count(*) FROM progress_grants WHERE player_id = $1`,
		"wallets":         `SELECT count(*) FROM wallets WHERE player_id = $1`,
		"ledger_entries":  `SELECT count(*) FROM ledger_entries WHERE player_id = $1`,
		"player_ratings":  `SELECT count(*) FROM player_ratings WHERE player_id = $1`,
		"rating_events":   `SELECT count(*) FROM rating_events WHERE player_id = $1`,
	} {
		var count int
		if err := pool.QueryRow(ctx, query, player).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if count != 0 {
			t.Errorf("%s: %d rows survive a rolled-back fan-out, want 0", name, count)
		}
	}

	// The same match, minus the ghost, must commit everything at once.
	match.Players = match.Players[:1]
	if err := sink.MatchFinished(ctx, match); err != nil {
		t.Fatalf("fan-out for a valid match: %v", err)
	}
	for name, query := range map[string]string{
		"player_progress": `SELECT count(*) FROM player_progress WHERE player_id = $1`,
		"wallets":         `SELECT count(*) FROM wallets WHERE player_id = $1`,
		"player_ratings":  `SELECT count(*) FROM player_ratings WHERE player_id = $1`,
	} {
		var count int
		if err := pool.QueryRow(ctx, query, player).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", name, err)
		}
		if count != 1 {
			t.Errorf("%s: %d rows after a successful fan-out, want 1", name, count)
		}
	}

	// Replaying the same result must not double-pay (idempotency unchanged).
	if err := sink.MatchFinished(ctx, match); err != nil {
		t.Fatalf("replayed fan-out: %v", err)
	}
	var ledger int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM ledger_entries WHERE player_id = $1`, player).Scan(&ledger); err != nil {
		t.Fatalf("count ledger: %v", err)
	}
	if ledger != 1 {
		t.Fatalf("ledger entries after replay = %d, want 1", ledger)
	}
}
