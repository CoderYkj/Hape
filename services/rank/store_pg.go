package rank

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
)

const queryTimeout = 3 * time.Second

// PG is the PostgreSQL Store.
//
// Two tables, two jobs: `player_ratings` is the current standing, `rating_events`
// is the append-only proof of how it got there. The event table's primary key
// (board, player_id, idempotency_key) is the idempotency guard: replaying a
// match cannot move the ladder twice.
type PG struct {
	pool *pgxpool.Pool
}

// NewPG builds the SQL-backed rating store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// RatingOf implements Store.
//
// The interface has no error return because matchmaking calls it on the hot path
// and can live with the default. A database failure therefore degrades to
// DefaultRating rather than failing the match: documented in ADR 0006 as debt to
// fix when Store gains a context and an error.
func (s *PG) RatingOf(ctx context.Context, board, playerID string) int64 {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var rating int64
	err := s.pool.QueryRow(ctx,
		`SELECT rating FROM player_ratings WHERE board = $1 AND player_id = $2`,
		board, playerID).Scan(&rating)
	if err != nil {
		return DefaultRating
	}
	return rating
}

// Apply implements Store.
func (s *PG) Apply(ctx context.Context, board, playerID, idempotencyKey string, won bool) (Rating, int64, bool, error) {
	if idempotencyKey == "" {
		return Rating{}, 0, false, apierr.Invalid("idempotency key is required")
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var (
		result  Rating
		delta   int64
		applied bool
	)
	err := pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Create the row first (no-op if present), then lock it. Locking order is
		// (insert, select for update) so two concurrent applies for one player
		// serialize instead of both reading the same old rating.
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_ratings (board, player_id, rating) VALUES ($1, $2, $3)
			 ON CONFLICT (board, player_id) DO NOTHING`,
			board, playerID, DefaultRating); err != nil {
			if pg.IsForeignKeyViolation(err) {
				return apierr.Invalid("unknown player_id: a rating cannot be created for a player that does not exist").
					WithDetail("player_id", playerID)
			}
			return err
		}

		var current Rating
		if err := tx.QueryRow(ctx,
			`SELECT rating, wins, losses FROM player_ratings
			  WHERE board = $1 AND player_id = $2 FOR UPDATE`,
			board, playerID).Scan(&current.Rating, &current.Wins, &current.Losses); err != nil {
			return err
		}
		current.PlayerID, current.Board = playerID, board

		step := WinDelta
		if won {
			current.Wins++
		} else {
			step = LossDelta
			current.Losses++
		}
		next := current.Rating + step
		if next < MinRating {
			next = MinRating
		}

		tag, err := tx.Exec(ctx,
			`INSERT INTO rating_events (board, player_id, idempotency_key, delta, rating_after)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (board, player_id, idempotency_key) DO NOTHING`,
			board, playerID, idempotencyKey, step, next)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			// The match was already applied to this player: report no movement.
			// The row lock is released by the rollback below.
			existing, err := s.rating(ctx, tx, board, playerID)
			if err != nil {
				return err
			}
			result, delta, applied = existing, 0, false
			return nil
		}

		if _, err := tx.Exec(ctx,
			`UPDATE player_ratings SET rating = $3, wins = $4, losses = $5, updated_at = now()
			  WHERE board = $1 AND player_id = $2`,
			board, playerID, next, current.Wins, current.Losses); err != nil {
			return err
		}

		current.Rating = next
		current.UpdatedAt = time.Now().UTC()
		result, delta, applied = current, step, true
		return nil
	})
	if err != nil {
		return Rating{}, 0, false, err
	}
	return result, delta, applied, nil
}

// Top implements Store.
func (s *PG) Top(ctx context.Context, board string, limit int) (Leaderboard, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	if limit <= 0 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx,
		`SELECT player_id, rating, wins, losses
		   FROM player_ratings
		  WHERE board = $1
		  ORDER BY rating DESC, player_id ASC
		  LIMIT $2`, board, limit)
	if err != nil {
		return Leaderboard{}, err
	}
	defer rows.Close()

	entries := []Entry{}
	for rows.Next() {
		var entry Entry
		if err := rows.Scan(&entry.PlayerID, &entry.Rating, &entry.Wins, &entry.Losses); err != nil {
			return Leaderboard{}, err
		}
		entry.Rank = int32(len(entries) + 1)
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return Leaderboard{}, err
	}
	return Leaderboard{
		Board:     board,
		Season:    "s0",
		Entries:   entries,
		UpdatedAt: time.Now().UTC(),
	}, nil
}

func (s *PG) rating(ctx context.Context, db pg.DB, board, playerID string) (Rating, error) {
	var (
		rating    Rating
		updatedAt time.Time
	)
	if err := db.QueryRow(ctx,
		`SELECT rating, wins, losses, updated_at FROM player_ratings
		  WHERE board = $1 AND player_id = $2`,
		board, playerID).Scan(&rating.Rating, &rating.Wins, &rating.Losses, &updatedAt); err != nil {
		return Rating{}, err
	}
	rating.PlayerID, rating.Board = playerID, board
	rating.UpdatedAt = updatedAt.UTC()
	return rating, nil
}
