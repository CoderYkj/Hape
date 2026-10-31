package account_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
	"gamebackend/services/account"
)

// TestStoresAgree compares the memory and PostgreSQL account stores step by step.
// Player ids are opaque and store-specific, so the assertions compare behaviour
// (created flags, stability, error codes), not the ids themselves.
//
//	Skipped without a database, which is why CI cannot prove it (see 80-ci-cd.md):
//	BACKEND_TEST_DATABASE_URL=postgres://... go test ./services/account/
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

	// A unique external id per run keeps concurrent runs from colliding.
	externalID := "ext_equiv_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM players WHERE external_id = $1`, externalID)
	})

	mem := account.NewMemory()
	sqlStore := account.NewPG(pool)

	for name, store := range map[string]account.Store{"memory": mem, "postgres": sqlStore} {
		t.Run(name, func(t *testing.T) {
			first, created, err := store.FindOrCreate(ctx, externalID, "Equiv")
			if err != nil {
				t.Fatalf("first login: %v", err)
			}
			if !created {
				t.Fatal("first login should create the player")
			}
			if first.DisplayName != "Equiv" {
				t.Fatalf("display_name = %q, want Equiv", first.DisplayName)
			}

			// The same Nakama user logging in again must get the same player.
			second, created, err := store.FindOrCreate(ctx, externalID, "Equiv")
			if err != nil {
				t.Fatalf("second login: %v", err)
			}
			if created {
				t.Fatal("second login must not create a player")
			}
			if second.ID != first.ID {
				t.Fatalf("second login returned a different player: %q vs %q", second.ID, first.ID)
			}

			read, err := store.GetByID(ctx, first.ID)
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if read.ExternalID != externalID {
				t.Fatalf("external_id = %q, want %q", read.ExternalID, externalID)
			}

			renamed, err := store.UpdateDisplayName(ctx, first.ID, "Renamed")
			if err != nil {
				t.Fatalf("rename: %v", err)
			}
			if renamed.DisplayName != "Renamed" {
				t.Fatalf("display_name after rename = %q", renamed.DisplayName)
			}
			if !renamed.UpdatedAt.After(first.UpdatedAt) && !renamed.UpdatedAt.Equal(first.UpdatedAt) {
				t.Fatalf("updated_at went backwards: %v -> %v", first.UpdatedAt, renamed.UpdatedAt)
			}

			// Authentication resolves a verified identity to a player, and it must be
			// read-only: looking up an unknown identity reports not_found instead of
			// creating an account.
			byExternal, err := store.LookupByExternalID(ctx, externalID)
			if err != nil {
				t.Fatalf("LookupByExternalID: %v", err)
			}
			if byExternal.ID != first.ID {
				t.Fatalf("LookupByExternalID returned %q, want %q", byExternal.ID, first.ID)
			}
			if _, err := store.LookupByExternalID(ctx, "ext-does-not-exist"); apierr.As(err).Code != apierr.CodeNotFound {
				t.Fatalf("unknown identity: code = %q, want not_found", apierr.As(err).Code)
			}

			// Refusals must agree too.
			if _, err := store.GetByID(ctx, "p_missing"); apierr.As(err).Code != apierr.CodeNotFound {
				t.Fatalf("unknown player: code = %q, want not_found", apierr.As(err).Code)
			}
			if _, _, err := store.FindOrCreate(ctx, "", "x"); apierr.As(err).Code != apierr.CodeInvalidArgument {
				t.Fatalf("empty external_id: code = %q, want invalid_argument", apierr.As(err).Code)
			}
		})
	}
}
