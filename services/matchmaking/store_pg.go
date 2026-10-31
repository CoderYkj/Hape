package matchmaking

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
)

const queryTimeout = 3 * time.Second

// PG is the PostgreSQL Store.
//
// The queue's central rule lives in the database: the partial unique index
// matchmaking_one_waiting_ticket allows exactly one waiting ticket per
// (queue, player), so "enqueue twice" cannot become "two matches" even if two
// requests race past the application check.
type PG struct {
	pool *pgxpool.Pool
}

// NewPG builds the SQL-backed queue store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// Create implements Store.
func (s *PG) Create(ctx context.Context, ticket Ticket) (Ticket, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	if ticket.Party == nil {
		ticket.Party = []string{}
	}
	id := "q_" + strconv.FormatInt(time.Now().UnixNano(), 36)

	var (
		createdAt time.Time
		updatedAt time.Time
	)
	err := s.pool.QueryRow(ctx,
		`INSERT INTO matchmaking_tickets (id, player_id, queue, mmr, party, status)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING created_at, updated_at`,
		id, ticket.PlayerID, ticket.Queue, ticket.MMR, ticket.Party, StatusWaiting).
		Scan(&createdAt, &updatedAt)
	if err != nil {
		if pg.IsForeignKeyViolation(err) {
			return Ticket{}, apierr.Invalid("unknown player_id: a ticket must belong to an existing player").
				WithDetail("player_id", ticket.PlayerID)
		}
		if pg.IsUniqueViolation(err) {
			return Ticket{}, apierr.Conflict("player already has a waiting ticket in this queue").
				WithDetail("queue", ticket.Queue)
		}
		return Ticket{}, err
	}

	ticket.ID = id
	ticket.Status = StatusWaiting
	ticket.CreatedAt = createdAt.UTC()
	ticket.UpdatedAt = updatedAt.UTC()
	return ticket, nil
}

// Get implements Store.
func (s *PG) Get(ctx context.Context, id string) (Ticket, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return s.get(ctx, s.pool, id)
}

// FindWaitingByPlayer implements Store.
//
// No error return: the interface treats "lookup failed" as "nothing waiting", and
// the create path then hits the unique index, which is the real guard. Documented
// as debt in ADR 0006.
func (s *PG) FindWaitingByPlayer(ctx context.Context, queue, playerID string) (Ticket, bool) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	ticket, err := s.scanTicket(s.pool.QueryRow(ctx,
		`SELECT id, player_id, party, queue, mmr, status, coalesce(match_id, ''), created_at, updated_at
		   FROM matchmaking_tickets
		  WHERE queue = $1 AND player_id = $2 AND status = $3
		  ORDER BY created_at DESC
		  LIMIT 1`, queue, playerID, StatusWaiting))
	if err != nil {
		return Ticket{}, false
	}
	return ticket, true
}

// Waiting implements Store.
func (s *PG) Waiting(ctx context.Context, queue string) int {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var count int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM matchmaking_tickets WHERE queue = $1 AND status = $2`,
		queue, StatusWaiting).Scan(&count); err != nil {
		return 0
	}
	return count
}

// Pop implements Store.
//
// The waiting rows are locked before selection, so two matchmakers popping the
// same queue cannot hand the same player to two matches.
func (s *PG) Pop(ctx context.Context, queue string, count int, band int64, matchID string) ([]Ticket, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var chosen []Ticket
	err := pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, player_id, party, queue, mmr, status, coalesce(match_id, ''), created_at, updated_at
			   FROM matchmaking_tickets
			  WHERE queue = $1 AND status = $2
			  ORDER BY created_at, id
			  FOR UPDATE`, queue, StatusWaiting)
		if err != nil {
			return err
		}
		waiting := []Ticket{}
		for rows.Next() {
			var ticket Ticket
			if err := rows.Scan(&ticket.ID, &ticket.PlayerID, &ticket.Party, &ticket.Queue,
				&ticket.MMR, &ticket.Status, &ticket.MatchID, &ticket.CreatedAt, &ticket.UpdatedAt); err != nil {
				rows.Close()
				return err
			}
			waiting = append(waiting, ticket)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(waiting) == 0 {
			return ErrNoWaiting
		}

		// Same band rule as the memory store: the oldest ticket anchors the match,
		// and only tickets within `band` MMR of it are eligible.
		anchor := waiting[0].MMR
		picked := make([]Ticket, 0, count)
		for _, ticket := range waiting {
			if len(picked) >= count {
				break
			}
			if abs64(ticket.MMR-anchor) <= band {
				picked = append(picked, ticket)
			}
		}
		// A one-player "match" is worse than waiting: leave the queue untouched
		// (the transaction rolls back and the locks are released).
		if len(picked) < 2 {
			return ErrNoWaiting
		}

		for i := range picked {
			if _, err := tx.Exec(ctx,
				`UPDATE matchmaking_tickets SET status = $2, match_id = $3, updated_at = now()
				  WHERE id = $1`, picked[i].ID, StatusMatched, matchID); err != nil {
				return err
			}
			picked[i].Status = StatusMatched
			picked[i].MatchID = matchID
		}
		chosen = picked
		return nil
	})
	if err != nil {
		return nil, err
	}
	return chosen, nil
}

func (s *PG) get(ctx context.Context, db pg.DB, id string) (Ticket, error) {
	ticket, err := s.scanTicket(db.QueryRow(ctx,
		`SELECT id, player_id, party, queue, mmr, status, coalesce(match_id, ''), created_at, updated_at
		   FROM matchmaking_tickets WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Ticket{}, apierr.NotFound("ticket not found").WithDetail("ticket_id", id)
	}
	return ticket, err
}

func (s *PG) scanTicket(row pgx.Row) (Ticket, error) {
	var (
		ticket    Ticket
		createdAt time.Time
		updatedAt time.Time
	)
	if err := row.Scan(&ticket.ID, &ticket.PlayerID, &ticket.Party, &ticket.Queue,
		&ticket.MMR, &ticket.Status, &ticket.MatchID, &createdAt, &updatedAt); err != nil {
		return Ticket{}, err
	}
	ticket.CreatedAt = createdAt.UTC()
	ticket.UpdatedAt = updatedAt.UTC()
	if ticket.Party == nil {
		ticket.Party = []string{}
	}
	return ticket, nil
}
