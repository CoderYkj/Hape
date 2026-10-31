package team_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
	"gamebackend/services/team"
)

// TestStoresAgree: team membership rules must be identical in both stores -
// including "one team per player", which the database enforces with an index.
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
	owner := "p_own_" + suffix
	member := "p_mem_" + suffix
	for _, id := range []string{owner, member} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO players (id, external_id, display_name) VALUES ($1, $2, $1) ON CONFLICT DO NOTHING`,
			id, "ext-"+id); err != nil {
			t.Fatalf("seed player: %v", err)
		}
	}
	t.Cleanup(func() {
		for _, id := range []string{owner, member} {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM team_members WHERE player_id = $1`, id)
			_, _ = pool.Exec(context.Background(), `DELETE FROM teams WHERE owner_id = $1`, id)
			_, _ = pool.Exec(context.Background(), `DELETE FROM players WHERE id = $1`, id)
		}
	})

	for _, tc := range []struct {
		name  string
		store team.Store
	}{
		{"memory", team.NewMemory()},
		{"postgres", team.NewPG(pool)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, err := tc.store.Create(owner, "squad")
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if created.OwnerID != owner || len(created.Members) != 1 || created.Members[0].Role != "owner" {
				t.Fatalf("unexpected team: %+v", created)
			}

			joined, applied, err := tc.store.Join(created.ID, member)
			if err != nil {
				t.Fatalf("join: %v", err)
			}
			if !applied || len(joined.Members) != 2 {
				t.Fatalf("join should apply: applied=%v members=%d", applied, len(joined.Members))
			}

			again, applied, err := tc.store.Join(created.ID, member)
			if err != nil {
				t.Fatalf("replay join: %v", err)
			}
			if applied || len(again.Members) != 2 {
				t.Fatalf("replay join must be a no-op: applied=%v members=%d", applied, len(again.Members))
			}

			read, err := tc.store.Get(created.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if read.Members[0].Role != "owner" {
				t.Fatalf("owner must be first, got %+v", read.Members)
			}

			// A player belongs to one team: a second team by the same owner is refused.
			if _, err := tc.store.Create(owner, "second"); apierr.As(err).Code != apierr.CodeFailedPrecondition {
				t.Fatalf("second team: code = %q, want failed_precondition", apierr.As(err).Code)
			}

			if _, err := tc.store.Get("t_missing"); apierr.As(err).Code != apierr.CodeNotFound {
				t.Fatalf("unknown team: code = %q, want not_found", apierr.As(err).Code)
			}
		})
	}
}
