package rank_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
	"gamebackend/services/rank"
)

// TestStoresAgree pins the ladder arithmetic and its idempotency: same wins, same
// losses, same replay, same floor, same leaderboard.
func TestStoresAgree(t *testing.T) {
	dsn := os.Getenv("BACKEND_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BACKEND_TEST_DATABASE_URL to run the PostgreSQL/memory equality test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()

	player := "p_rank_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	// A private board per run: the database is shared with local runs and other
	// tests, so the leaderboard comparison must not see their rows.
	board := "equiv_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM rating_events WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM player_ratings WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM players WHERE id = $1`, player)
	})
	if _, err := pool.Exec(ctx,
		`INSERT INTO players (id, external_id, display_name) VALUES ($1, $2, $1) ON CONFLICT DO NOTHING`,
		player, "ext-"+player); err != nil {
		t.Fatalf("seed player: %v", err)
	}

	mem := rank.NewMemory()
	sqlStore := rank.NewPG(pool)

	memOut := script(t, ctx, mem, board, player)
	sqlOut := script(t, ctx, sqlStore, board, player)

	if len(memOut) != len(sqlOut) {
		t.Fatalf("step count: memory=%d sql=%d", len(memOut), len(sqlOut))
	}
	for i := range memOut {
		m, s := memOut[i], sqlOut[i]
		if m.rating != s.rating || m.delta != s.delta || m.applied != s.applied || m.errCode != s.errCode {
			t.Errorf("step %d: memory=%+v sql=%+v", i, m, s)
		}
	}

	memBoard, err := mem.Top(ctx, board, 50)
	if err != nil {
		t.Fatalf("memory leaderboard: %v", err)
	}
	sqlBoard, err := sqlStore.Top(ctx, board, 50)
	if err != nil {
		t.Fatalf("sql leaderboard: %v", err)
	}
	if len(memBoard.Entries) != len(sqlBoard.Entries) {
		t.Fatalf("leaderboard size: memory=%d sql=%d", len(memBoard.Entries), len(sqlBoard.Entries))
	}
	for i := range memBoard.Entries {
		if memBoard.Entries[i].PlayerID != sqlBoard.Entries[i].PlayerID ||
			memBoard.Entries[i].Rating != sqlBoard.Entries[i].Rating ||
			memBoard.Entries[i].Rank != sqlBoard.Entries[i].Rank {
			t.Errorf("entry %d: memory=%+v sql=%+v", i, memBoard.Entries[i], sqlBoard.Entries[i])
		}
	}
}

type outcome struct {
	rating  int64
	delta   int64
	applied bool
	errCode apierr.Code
}

func script(t *testing.T, ctx context.Context, store rank.Store, board, player string) []outcome {
	t.Helper()
	var out []outcome
	record := func(r rank.Rating, delta int64, applied bool, err error) {
		step := outcome{rating: r.Rating, delta: delta, applied: applied}
		if err != nil {
			step.errCode = apierr.As(err).Code
		}
		out = append(out, step)
	}

	record(rank.Rating{Rating: store.RatingOf(ctx, board, player)}, 0, false, nil)

	r, delta, applied, err := store.Apply(ctx, board, player, "k1", true)
	record(r, delta, applied, err)

	// Replay: no movement, applied=false.
	r, delta, applied, err = store.Apply(ctx, board, player, "k1", true)
	record(r, delta, applied, err)

	r, delta, applied, err = store.Apply(ctx, board, player, "k2", false)
	record(r, delta, applied, err)

	r, delta, applied, err = store.Apply(ctx, board, player, "", true)
	record(r, delta, applied, err)

	// Drive the player to the floor: 62 losses from ~1008 must clamp at MinRating.
	for i := 0; i < 62; i++ {
		key := "floor-" + strconv.Itoa(i)
		r, delta, applied, err = store.Apply(ctx, board, player, key, false)
		if err != nil {
			t.Fatalf("loss %d: %v", i, err)
		}
	}
	record(r, delta, applied, err)

	return out
}

// TestPGRejectsRatingForUnknownPlayer documents the foreign-key divergence.
func TestPGRejectsRatingForUnknownPlayer(t *testing.T) {
	dsn := os.Getenv("BACKEND_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BACKEND_TEST_DATABASE_URL to run the PostgreSQL store tests")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()

	ghost := "p_ghost_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	_, _, _, err = rank.NewPG(pool).Apply(ctx, rank.DefaultBoard, ghost, "k_ghost", true)
	if err == nil {
		t.Fatal("rating an unknown player should be refused")
	}
	if code := apierr.As(err).Code; code != apierr.CodeInvalidArgument {
		t.Fatalf("code = %q, want invalid_argument (err: %v)", code, err)
	}
}
