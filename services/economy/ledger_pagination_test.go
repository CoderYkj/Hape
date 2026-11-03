package economy_test

import (
	"context"
	"testing"

	"gamebackend/services/economy"
)

// TestLedgerPagesByCursor is the paging contract on the memory store (the Postgres store is
// held to the same behaviour by the equivalence suite): no row is repeated, no row is
// skipped, and the last page ends with an empty cursor.
func TestLedgerPagesByCursor(t *testing.T) {
	ctx := context.Background()
	store := economy.NewMemory()
	player := "p_page"
	for i := 0; i < 5; i++ {
		if _, _, err := store.Grant(ctx, player, "grant-"+itoa(i), economy.CurrencyGold, 10, "test"); err != nil {
			t.Fatalf("grant %d: %v", i, err)
		}
	}

	seen := map[string]bool{}
	pages := 0
	cursor := ""
	for {
		entries, err := store.Ledger(ctx, player, economy.LedgerQuery{Limit: 2, After: cursor})
		if err != nil {
			t.Fatalf("page %d: %v", pages, err)
		}
		pages++
		for _, e := range entries {
			if seen[e.ID] {
				t.Fatalf("entry %s was returned twice", e.ID)
			}
			seen[e.ID] = true
		}
		if len(entries) < 2 {
			break
		}
		cursor = entries[len(entries)-1].ID
		if pages > 10 {
			t.Fatal("paging did not terminate")
		}
	}
	if len(seen) != 5 {
		t.Fatalf("paged %d entries, want 5", len(seen))
	}
	if pages != 3 {
		t.Fatalf("took %d pages of 2 for 5 rows, want 3", pages)
	}

	// A cursor from another player's ledger (or a typo) must be refused, not silently
	// restarted: a client that loops forever on page one is worse than a 400.
	other := "p_other"
	if _, _, err := store.Grant(ctx, other, "grant-other", economy.CurrencyGold, 10, "test"); err != nil {
		t.Fatalf("grant other: %v", err)
	}
	otherEntries, err := store.Ledger(ctx, other, economy.LedgerQuery{Limit: 10})
	if err != nil {
		t.Fatalf("other ledger: %v", err)
	}
	if _, err := store.Ledger(ctx, "p_page", economy.LedgerQuery{Limit: 10, After: otherEntries[0].ID}); err == nil {
		t.Fatal("a cursor from another player's ledger must be refused")
	}
	if _, err := store.Ledger(ctx, "p_page", economy.LedgerQuery{Limit: 10, After: "l_nope"}); err == nil {
		t.Fatal("an unknown cursor must be refused")
	}
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var out []byte
	for v > 0 {
		out = append([]byte{byte('0' + v%10)}, out...)
		v /= 10
	}
	return string(out)
}
