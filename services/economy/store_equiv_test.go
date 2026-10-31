package economy_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
	"gamebackend/services/economy"
)

// outcome is one step of the scripted scenario, recorded so the two stores can
// be compared step by step instead of only at the end.
type outcome struct {
	applied  bool
	balances map[string]int64
	errCode  apierr.Code
	errText  string
}

// TestStoresAgree is the whole point of having an interface: the memory store and
// the PostgreSQL store must answer the same way to the same sequence of
// requests, including the refusals.
//
// Skipped unless a database is reachable, because CI has no PostgreSQL service
// (see .harness/rules/80-ci-cd.md):
//
//	BACKEND_TEST_DATABASE_URL=postgres://gamebackend:gamebackend@127.0.0.1:55432/gamebackend?sslmode=disable go test ./services/economy/
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

	// A fresh player per run: the test never needs to clean the whole table, and
	// two concurrent runs cannot collide.
	player := "p_equiv_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	cleanup := func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM ledger_entries WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM wallets WHERE player_id = $1`, player)
		_, _ = pool.Exec(context.Background(), `DELETE FROM players WHERE id = $1`, player)
	}
	defer cleanup()

	// Seed the player. In production a wallet only exists for an account that
	// logged in first: ledger_entries.player_id carries a foreign key, and this
	// test follows the real ordering rather than working around the constraint.
	if _, err := pool.Exec(ctx,
		`INSERT INTO players (id, external_id, display_name) VALUES ($1, $2, $1) ON CONFLICT DO NOTHING`,
		player, "ext-"+player); err != nil {
		t.Fatalf("seed player: %v", err)
	}

	mem := economy.NewMemory()
	sqlStore := economy.NewPG(pool)

	memOut := script(t, mem, player)
	sqlOut := script(t, sqlStore, player)

	if len(memOut) != len(sqlOut) {
		t.Fatalf("step count differs: memory=%d sql=%d", len(memOut), len(sqlOut))
	}
	for i := range memOut {
		m, s := memOut[i], sqlOut[i]
		if m.applied != s.applied {
			t.Errorf("step %d: applied memory=%v sql=%v", i, m.applied, s.applied)
		}
		if m.errCode != s.errCode {
			t.Errorf("step %d: error code memory=%q sql=%q (sql said: %s)", i, m.errCode, s.errCode, s.errText)
		}
		if len(m.balances) != len(s.balances) {
			t.Errorf("step %d: balances memory=%v sql=%v", i, m.balances, s.balances)
			continue
		}
		for currency, want := range m.balances {
			if got := s.balances[currency]; got != want {
				t.Errorf("step %d: balance[%s] memory=%d sql=%d", i, currency, want, got)
			}
		}
	}

	// The ledger is the audit trail; amounts, signs and reasons must match even
	// though the two stores mint their own ids.
	memLedger, err := mem.Ledger(player)
	if err != nil {
		t.Fatalf("memory ledger: %v", err)
	}
	sqlLedger, err := sqlStore.Ledger(player)
	if err != nil {
		t.Fatalf("sql ledger: %v", err)
	}
	if len(memLedger) != len(sqlLedger) {
		t.Fatalf("ledger length memory=%d sql=%d", len(memLedger), len(sqlLedger))
	}
	for i := range memLedger {
		if memLedger[i].Amount != sqlLedger[i].Amount ||
			memLedger[i].Currency != sqlLedger[i].Currency ||
			memLedger[i].Reason != sqlLedger[i].Reason {
			t.Errorf("ledger entry %d differs: memory=%+v sql=%+v", i, memLedger[i], sqlLedger[i])
		}
		if len(sqlLedger[i].ID) < 2 || sqlLedger[i].ID[:2] != "l_" {
			t.Errorf("ledger entry %d: id %q does not match the l_<base36> shape", i, sqlLedger[i].ID)
		}
	}
}

// script runs the same requests against one store and records what happened.
func script(t *testing.T, store economy.Store, player string) []outcome {
	t.Helper()

	var out []outcome
	record := func(applied bool, wallet economy.Wallet, err error) {
		// Copy the map: the memory store hands back its live map, so recording the
		// reference would make every earlier step appear to hold the latest state.
		balances := make(map[string]int64, len(wallet.Balances))
		for currency, balance := range wallet.Balances {
			balances[currency] = balance
		}
		step := outcome{applied: applied, balances: balances}
		if err != nil {
			step.errCode = apierr.As(err).Code
			step.errText = err.Error()
		}
		out = append(out, step)
	}

	wallet, err := store.Wallet(player)
	if err != nil {
		t.Fatalf("wallet: %v", err)
	}
	out = append(out, outcome{balances: wallet.Balances})

	w, applied, err := store.Grant(player, "k1", economy.CurrencyGold, 100, "match_reward")
	record(applied, w, err)

	// Same key, same player: a replay, not a second payout.
	w, applied, err = store.Grant(player, "k1", economy.CurrencyGold, 100, "match_reward")
	record(applied, w, err)

	w, applied, err = store.Spend(player, "k2", economy.CurrencyGold, 60, "skin-01")
	record(applied, w, err)

	// Spending currency the player has never held.
	w, applied, err = store.Spend(player, "k3", economy.CurrencyGems, 100, "skin-02")
	record(applied, w, err)

	// Spending more than the balance.
	w, applied, err = store.Spend(player, "k4", economy.CurrencyGold, 1000, "skin-03")
	record(applied, w, err)

	// Invalid inputs must be refused before any store is touched.
	w, applied, err = store.Grant(player, "", economy.CurrencyGold, 10, "match_reward")
	record(applied, w, err)
	w, applied, err = store.Grant(player, "k5", "dogecoin", 10, "match_reward")
	record(applied, w, err)
	w, applied, err = store.Grant(player, "k6", economy.CurrencyGold, 0, "match_reward")
	record(applied, w, err)

	w, applied, err = store.Grant(player, "k7", economy.CurrencyGold, 5, "match_reward")
	record(applied, w, err)

	return out
}

// TestPGRejectsMoneyForUnknownPlayer documents a deliberate divergence: the
// database enforces the players foreign key, the memory store cannot. The SQL
// store turns that into invalid_argument with the player id attached, instead of
// leaking a 500.
func TestPGRejectsMoneyForUnknownPlayer(t *testing.T) {
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

	store := economy.NewPG(pool)
	ghost := "p_ghost_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	_, _, err = store.Grant(ghost, "k_ghost", economy.CurrencyGold, 10, "match_reward")
	if err == nil {
		t.Fatal("granting money to a player that does not exist should be refused")
	}
	if code := apierr.As(err).Code; code != apierr.CodeInvalidArgument {
		t.Fatalf("error code = %q, want invalid_argument (err: %v)", code, err)
	}
}

// TestPGRejectsMalformedDSN keeps the startup path honest: a bad DSN is an error
// at Open, not a crash on the first query.
func TestPGRejectsMalformedDSN(t *testing.T) {
	if _, err := pg.Open(context.Background(), "postgres://nope::bad"); err == nil {
		t.Fatal("pg.Open accepted a malformed DSN")
	}
}
