package progress_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
	"gamebackend/services/progress"
)

// TestStoresAgree: the memory store and the PostgreSQL store must answer the same
// way to the same grants, including the "this key was already used" path that
// survives a restart only on one of them.
//
//	BACKEND_TEST_DATABASE_URL=postgres://... go test ./services/progress/
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

	player := "p_prog_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM progress_grants WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM player_progress WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM players WHERE id = $1`, player)
	})
	// XP carries a foreign key to players, like money does.
	if _, err := pool.Exec(ctx,
		`INSERT INTO players (id, external_id, display_name) VALUES ($1, $2, $1) ON CONFLICT DO NOTHING`,
		player, "ext-"+player); err != nil {
		t.Fatalf("seed player: %v", err)
	}

	mem := progress.NewMemory()
	sqlStore := progress.NewPG(pool)

	steps := []string{
		"get fresh",
		"grant 140",
		"replay 140",
		"grant 900 (crosses level 2)",
		"grant with empty key",
		"grant zero xp",
		"get final",
	}
	memOut := script(t, ctx, mem, player)
	sqlOut := script(t, ctx, sqlStore, player)

	if len(memOut) != len(sqlOut) || len(memOut) != len(steps) {
		t.Fatalf("step count: memory=%d sql=%d want=%d", len(memOut), len(sqlOut), len(steps))
	}
	for i := range memOut {
		m, s := memOut[i], sqlOut[i]
		if m.applied != s.applied {
			t.Errorf("%s: applied memory=%v sql=%v", steps[i], m.applied, s.applied)
		}
		if m.errCode != s.errCode {
			t.Errorf("%s: code memory=%q sql=%q (%s)", steps[i], m.errCode, s.errCode, s.errText)
		}
		if m.totalXP != s.totalXP || m.level != s.level {
			t.Errorf("%s: memory xp=%d lvl=%d, sql xp=%d lvl=%d", steps[i], m.totalXP, m.level, s.totalXP, s.level)
		}
	}
}

type outcome struct {
	applied bool
	totalXP int64
	level   int32
	errCode apierr.Code
	errText string
}

func script(t *testing.T, ctx context.Context, store progress.Store, player string) []outcome {
	t.Helper()
	var out []outcome
	record := func(p progress.Progress, applied bool, err error) {
		step := outcome{applied: applied, totalXP: p.TotalXP, level: p.Level}
		if err != nil {
			step.errCode = apierr.As(err).Code
			step.errText = err.Error()
		}
		out = append(out, step)
	}

	p, err := store.Get(ctx, player)
	record(p, false, err)

	p, applied, err := store.GrantXP(ctx, player, "k1", 140, "match_reward")
	record(p, applied, err)

	p, applied, err = store.GrantXP(ctx, player, "k1", 140, "match_reward")
	record(p, applied, err)

	p, applied, err = store.GrantXP(ctx, player, "k2", 900, "match_reward")
	record(p, applied, err)

	p, applied, err = store.GrantXP(ctx, player, "", 10, "match_reward")
	record(p, applied, err)

	p, applied, err = store.GrantXP(ctx, player, "k3", 0, "match_reward")
	record(p, applied, err)

	p, err = store.Get(ctx, player)
	record(p, false, err)

	return out
}

// TestPGRejectsXPForUnknownPlayer documents the foreign-key divergence: the
// database refuses XP for a player that does not exist, the memory store cannot.
func TestPGRejectsXPForUnknownPlayer(t *testing.T) {
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
	_, _, err = progress.NewPG(pool).GrantXP(ctx, ghost, "k_ghost", 10, "match_reward")
	if err == nil {
		t.Fatal("granting xp to an unknown player should be refused")
	}
	if code := apierr.As(err).Code; code != apierr.CodeInvalidArgument {
		t.Fatalf("code = %q, want invalid_argument (err: %v)", code, err)
	}
}
