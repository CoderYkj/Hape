package economy

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
)

// movementTimeout bounds one wallet movement. Money operations are short; a
// query that takes longer is a symptom, and holding a connection forever makes
// it worse.
const movementTimeout = 3 * time.Second

// PG is the PostgreSQL Store.
//
// It implements the same Store interface as Memory, which is the whole point:
// the service layer and the HTTP contract do not know or care which one is
// mounted.
type PG struct {
	pool *pgxpool.Pool
}

// NewPG builds the SQL-backed economy store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// Wallet implements Store.
func (s *PG) Wallet(playerID string) (Wallet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), movementTimeout)
	defer cancel()
	return s.wallet(ctx, s.pool, playerID)
}

// Grant implements Store.
func (s *PG) Grant(playerID, idempotencyKey, currency string, amount int64, reason string) (Wallet, bool, error) {
	if err := validateMovement(idempotencyKey, currency, amount); err != nil {
		return Wallet{}, false, err
	}
	if reason == "" {
		return Wallet{}, false, apierr.Invalid("reason is required").WithDetail("field", "reason")
	}
	return s.movement(playerID, idempotencyKey, currency, amount, reason)
}

// Spend implements Store.
func (s *PG) Spend(playerID, idempotencyKey, currency string, amount int64, itemID string) (Wallet, bool, error) {
	if err := validateMovement(idempotencyKey, currency, amount); err != nil {
		return Wallet{}, false, err
	}
	if itemID == "" {
		return Wallet{}, false, apierr.Invalid("item_id is required").WithDetail("field", "item_id")
	}
	return s.movement(playerID, idempotencyKey, currency, -amount, "purchase:"+itemID)
}

// Ledger implements Store.
func (s *PG) Ledger(playerID string) ([]LedgerEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), movementTimeout)
	defer cancel()

	rows, err := s.pool.Query(ctx,
		`SELECT id, currency, amount, reason, created_at
		   FROM ledger_entries
		  WHERE player_id = $1
		  ORDER BY id`, playerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []LedgerEntry{}
	for rows.Next() {
		var (
			id        int64
			currency  string
			amount    int64
			reason    string
			createdAt time.Time
		)
		if err := rows.Scan(&id, &currency, &amount, &reason, &createdAt); err != nil {
			return nil, err
		}
		out = append(out, LedgerEntry{
			ID:        ledgerID(id),
			PlayerID:  playerID,
			Currency:  currency,
			Amount:    amount,
			Reason:    reason,
			CreatedAt: createdAt.UTC(),
		})
	}
	return out, rows.Err()
}

// movement applies one signed change to one currency.
//
// The ordering is deliberate: the ledger row goes in first, because its unique
// (player_id, idempotency_key) index is what makes a replay a no-op. A SELECT
// "has this been applied?" check before the write would be a race, not a guard.
func (s *PG) movement(playerID, idempotencyKey, currency string, delta int64, reason string) (Wallet, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), movementTimeout)
	defer cancel()

	var (
		wallet  Wallet
		applied bool
	)
	err := pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (player_id, currency, amount, reason, idempotency_key)
			 VALUES ($1, $2, $3, $4, $5)`,
			playerID, currency, delta, reason, idempotencyKey); err != nil {
			// A movement must reference a real player. The memory store cannot
			// enforce that (it has no account table); the database can, and does.
			// Turn it into a domain answer instead of a 500.
			if pg.IsForeignKeyViolation(err) {
				return apierr.Invalid("unknown player_id: money cannot reference a player that does not exist").
					WithDetail("player_id", playerID)
			}
			if pg.IsUniqueViolation(err) {
				// Same key, same player: this movement already happened. The
				// statement aborted the transaction, so the wallet cannot be read
				// here - the caller reads it after the rollback.
				return errReplay
			}
			return err
		}

		if delta > 0 {
			// A grant can upsert: the proposed row is positive, so the balance
			// CHECK holds whether the statement inserts or adds.
			if _, err := tx.Exec(ctx,
				`INSERT INTO wallets (player_id, currency, balance, updated_at)
				 VALUES ($1, $2, $3, now())
				 ON CONFLICT (player_id, currency) DO UPDATE
				    SET balance = wallets.balance + EXCLUDED.balance,
				        updated_at = now()`,
				playerID, currency, delta); err != nil {
				return err
			}
		} else {
			// A spend cannot upsert. PostgreSQL validates CHECK constraints on the
			// proposed row *before* conflict resolution, so an upsert carrying a
			// negative delta trips wallets_balance_check on the imaginary row
			// (0 - 60) instead of adding to the real balance. Guarded UPDATE it is:
			// no matching row (missing wallet or not enough funds) means refused.
			tag, err := tx.Exec(ctx,
				`UPDATE wallets
				    SET balance = balance + $3, updated_at = now()
				  WHERE player_id = $1 AND currency = $2 AND balance + $3 >= 0`,
				playerID, currency, delta)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return errInsufficientBalance
			}
		}

		current, err := s.wallet(ctx, tx, playerID)
		if err != nil {
			return err
		}
		wallet, applied = current, true
		return nil
	})
	switch {
	case errors.Is(err, errReplay):
		// A replay is a success with applied=false: return the wallet as it
		// stands now, which may have moved on since the original movement.
		current, err := s.wallet(ctx, s.pool, playerID)
		if err != nil {
			return Wallet{}, false, err
		}
		return current, false, nil
	case errors.Is(err, errInsufficientBalance):
		return Wallet{}, false, s.insufficientBalance(ctx, s.pool, playerID, currency, -delta)
	case err != nil:
		return Wallet{}, false, err
	}
	return wallet, applied, nil
}

// errReplay is an internal sentinel: the idempotency key was already used, and
// the aborted transaction means the wallet has to be read outside it.
var errReplay = errors.New("economy: idempotency key already applied")

// errInsufficientBalance is an internal sentinel: the CHECK constraint said no,
// and the caller turns it into a domain error with the numbers attached.
var errInsufficientBalance = errors.New("economy: insufficient balance")

func (s *PG) insufficientBalance(ctx context.Context, db pg.DB, playerID, currency string, required int64) error {
	var balance int64
	err := db.QueryRow(ctx,
		`SELECT balance FROM wallets WHERE player_id = $1 AND currency = $2`,
		playerID, currency).Scan(&balance)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return apierr.Precondition("insufficient balance").
		WithDetail("currency", currency).
		WithDetail("balance", strconv.FormatInt(balance, 10)).
		WithDetail("required", strconv.FormatInt(required, 10))
}

func (s *PG) wallet(ctx context.Context, db pg.DB, playerID string) (Wallet, error) {
	rows, err := db.Query(ctx,
		`SELECT currency, balance, updated_at FROM wallets WHERE player_id = $1`, playerID)
	if err != nil {
		return Wallet{}, err
	}
	defer rows.Close()

	wallet := Wallet{PlayerID: playerID, Balances: map[string]int64{}}
	for rows.Next() {
		var (
			currency  string
			balance   int64
			updatedAt time.Time
		)
		if err := rows.Scan(&currency, &balance, &updatedAt); err != nil {
			return Wallet{}, err
		}
		wallet.Balances[currency] = balance
		if updatedAt.After(wallet.UpdatedAt) {
			wallet.UpdatedAt = updatedAt.UTC()
		}
	}
	if err := rows.Err(); err != nil {
		return Wallet{}, err
	}
	if len(wallet.Balances) == 0 {
		// A player who has never earned anything still has a wallet; it is just
		// empty. Same answer as the memory store.
		wallet.UpdatedAt = time.Now().UTC()
	}
	if wallet.UpdatedAt.IsZero() {
		wallet.UpdatedAt = time.Now().UTC()
	}
	return wallet, nil
}

// ledgerID keeps the wire format stable across stores: the memory store mints
// "l_<base36>", so the SQL store formats its bigserial the same way instead of
// exposing a bare number.
func ledgerID(id int64) string { return "l_" + strconv.FormatInt(id, 36) }

// CurrencyList returns the supported currencies in a stable order. Handy for
// tests and for anything that wants to render a wallet consistently.
func CurrencyList() []string {
	out := make([]string, 0, len(allowedCurrencies))
	for c := range allowedCurrencies {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}
