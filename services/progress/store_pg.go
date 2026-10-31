package progress

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
)

const queryTimeout = 3 * time.Second

// PG is the PostgreSQL Store.
//
// Idempotency lives in `progress_grants` (migration 0002): a grant is a row, and
// the primary key (player_id, idempotency_key) is what makes a replay a no-op.
// The in-memory store used a map, which forgot everything on restart - exactly
// when a retry is most likely.
type PG struct {
	pool *pgxpool.Pool
}

// NewPG builds the SQL-backed progression store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// Get implements Store.
func (s *PG) Get(ctx context.Context, playerID string) (Progress, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var (
		totalXP   int64
		level     int32
		unlocks   []string
		updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx,
		`SELECT total_xp, level, unlocks, updated_at FROM player_progress WHERE player_id = $1`,
		playerID).Scan(&totalXP, &level, &unlocks, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		// No history is not an error: level 1, zero XP. Same answer as memory.
		return Progress{PlayerID: playerID, Level: 1, Unlocks: []string{}}, nil
	}
	if err != nil {
		return Progress{}, err
	}
	if unlocks == nil {
		unlocks = []string{}
	}
	return Progress{
		PlayerID:  playerID,
		Level:     level,
		TotalXP:   totalXP,
		Unlocks:   unlocks,
		UpdatedAt: updatedAt.UTC(),
	}, nil
}

// GrantXP implements Store.
func (s *PG) GrantXP(ctx context.Context, playerID, idempotencyKey string, xp int64, reason string) (Progress, bool, error) {
	if idempotencyKey == "" {
		return Progress{}, false, apierr.Invalid("idempotency key is required")
	}
	if xp <= 0 {
		return Progress{}, false, apierr.Invalid("xp must be greater than zero").WithDetail("field", "xp")
	}
	if reason == "" {
		return Progress{}, false, apierr.Invalid("reason is required").WithDetail("field", "reason")
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var (
		result  Progress
		applied bool
	)
	err := pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// ON CONFLICT DO NOTHING keeps the transaction alive on a replay (unlike a
		// unique violation), so the read below can happen in the same transaction.
		tag, err := tx.Exec(ctx,
			`INSERT INTO progress_grants (player_id, idempotency_key, xp, reason)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (player_id, idempotency_key) DO NOTHING`,
			playerID, idempotencyKey, xp, reason)
		if err != nil {
			if pg.IsForeignKeyViolation(err) {
				return apierr.Invalid("unknown player_id: xp cannot be granted to a player that does not exist").
					WithDetail("player_id", playerID)
			}
			return err
		}
		if tag.RowsAffected() == 0 {
			current, err := s.get(ctx, tx, playerID)
			if err != nil {
				return err
			}
			result, applied = current, false
			return nil
		}

		// total_xp is monotonic; the level curve stays in Go (levelFor), so a
		// second copy of "1000" never appears in SQL.
		var totalXP int64
		if err := tx.QueryRow(ctx,
			`INSERT INTO player_progress (player_id, total_xp, level, updated_at)
			 VALUES ($1, $2, 1, now())
			 ON CONFLICT (player_id) DO UPDATE
			    SET total_xp = player_progress.total_xp + EXCLUDED.total_xp,
			        updated_at = now()
			 RETURNING total_xp`,
			playerID, xp).Scan(&totalXP); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE player_progress SET level = $2 WHERE player_id = $1 AND level <> $2`,
			playerID, levelFor(totalXP)); err != nil {
			return err
		}

		current, err := s.get(ctx, tx, playerID)
		if err != nil {
			return err
		}
		result, applied = current, true
		return nil
	})
	if err != nil {
		return Progress{}, false, err
	}
	return result, applied, nil
}

func (s *PG) get(ctx context.Context, db pg.DB, playerID string) (Progress, error) {
	var (
		totalXP   int64
		level     int32
		unlocks   []string
		updatedAt time.Time
	)
	err := db.QueryRow(ctx,
		`SELECT total_xp, level, unlocks, updated_at FROM player_progress WHERE player_id = $1`,
		playerID).Scan(&totalXP, &level, &unlocks, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Progress{PlayerID: playerID, Level: 1, Unlocks: []string{}}, nil
	}
	if err != nil {
		return Progress{}, err
	}
	if unlocks == nil {
		unlocks = []string{}
	}
	return Progress{PlayerID: playerID, Level: level, TotalXP: totalXP, Unlocks: unlocks, UpdatedAt: updatedAt.UTC()}, nil
}
