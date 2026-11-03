package session

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

const queryTimeout = 3 * time.Second

// PG is the PostgreSQL Store.
//
// Two schema rules do the heavy lifting: sessions_one_per_match (a match gets one
// session, so a retried create cannot allocate a second server) and
// sessions_finished_has_result (a session cannot be marked finished without an
// accepted result id). The Update path keeps the state machine honest by loading,
// locking and writing back inside one transaction.
type PG struct {
	pool *pgxpool.Pool
}

// NewPG builds the SQL-backed session store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// Create implements Store.
func (s *PG) Create(ctx context.Context, session Session) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	// A brand new session has nothing to release yet; the columns default to it, and
	// the memory store normalises the same way so the two cannot drift.
	session.ReleasePending = false
	session.ReleaseAttempts = 0
	session.AllocatedAt = nil
	session.ReleasedAt = nil

	err := pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO sessions (id, match_id, state, queue, region, map,
			                       allocation_id, provider, address, port, result_id, runtime_token,
			                       created_at, ready_at, finished_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)`,
			session.ID, session.MatchID, session.State, session.Queue, session.Region, session.Map,
			nullify(session.AllocationID), nullify(session.Provider), nullify(session.Address),
			nullifyPort(session.Port), nullify(session.ResultID), nullify(session.RuntimeToken),
			session.CreatedAt, session.ReadyAt, session.FinishedAt); err != nil {
			if pg.IsUniqueViolation(err) {
				if pg.ConstraintName(err) == "sessions_one_per_match" {
					return apierr.Conflict("a session already exists for this match").
						WithDetail("match_id", session.MatchID)
				}
				return apierr.Conflict("session already exists").WithDetail("session_id", session.ID)
			}
			return err
		}
		for _, playerID := range session.Players {
			if _, err := tx.Exec(ctx,
				`INSERT INTO session_players (session_id, player_id) VALUES ($1, $2)
				 ON CONFLICT DO NOTHING`, session.ID, playerID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return session, nil
}

// Get implements Store.
func (s *PG) Get(ctx context.Context, id string) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return s.get(ctx, s.pool, id, false)
}

// GetByMatch implements Store.
func (s *PG) GetByMatch(ctx context.Context, matchID string) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	return s.getBy(ctx, s.pool, "match_id", matchID, false)
}

// Update implements Store.
//
// mutate runs against a locked snapshot. If it returns an error (an illegal state
// transition), nothing is written and the transaction rolls back.
func (s *PG) Update(ctx context.Context, id string, mutate func(*Session) error) (Session, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	var result Session
	err := pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		current, err := s.get(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if err := mutate(&current); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE sessions SET
			     state = $2, allocation_id = $3, provider = $4, address = $5, port = $6,
			     result_id = $7, ready_at = $8, finished_at = $9, runtime_token = $10,
			     release_pending = $11, release_attempts = $12,
			     allocated_at = $13, released_at = $14
			   WHERE id = $1`,
			current.ID, current.State, nullify(current.AllocationID), nullify(current.Provider),
			nullify(current.Address), nullifyPort(current.Port), nullify(current.ResultID),
			current.ReadyAt, current.FinishedAt, nullify(current.RuntimeToken),
			current.ReleasePending, current.ReleaseAttempts,
			current.AllocatedAt, current.ReleasedAt); err != nil {
			if pg.IsCheckViolation(err) {
				// e.g. finished without a result id: the schema refuses a state the
				// Go state machine should never have produced.
				return apierr.Precondition("invalid session state for this transition").
					WithDetail("state", current.State)
			}
			return err
		}
		result = current
		return nil
	})
	if err != nil {
		return Session{}, err
	}
	return result, nil
}

// PendingReleases implements ReleaseTracker.
func (s *PG) PendingReleases(ctx context.Context, limit int) ([]Session, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	query := `SELECT id, match_id, state, queue, region, map,
	                 coalesce(allocation_id, ''), coalesce(provider, ''), coalesce(address, ''),
	                 coalesce(port, 0), coalesce(result_id, ''), coalesce(runtime_token, ''), created_at, ready_at, finished_at,
	                 release_pending, release_attempts, allocated_at, released_at
	            FROM sessions WHERE release_pending
	           ORDER BY created_at, id`
	args := []any{}
	if limit > 0 {
		query += ` LIMIT $1`
		args = append(args, limit)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	pending := []Session{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		pending = append(pending, session)
	}
	return pending, rows.Err()
}

// ExpireStale implements Expirer.
//
// Only allocating and ready are swept: an in_progress match may legitimately be
// long, and killing it is worse than paying for a leaked server.
func (s *PG) ExpireStale(ctx context.Context, before time.Time) ([]Session, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	// Parameters must start at $1 and stay contiguous: PostgreSQL cannot infer the
	// type of a placeholder it never sees used.
	rows, err := s.pool.Query(ctx,
		`UPDATE sessions SET state = $2
		  WHERE state IN ($3, $4) AND created_at < $1
		  RETURNING id, match_id, state, queue, region, map, coalesce(allocation_id, ''),
		            coalesce(provider, ''), coalesce(address, ''), coalesce(port, 0),
		            coalesce(result_id, ''), coalesce(runtime_token, ''), created_at, ready_at, finished_at,
		            release_pending, release_attempts, allocated_at, released_at`,
		before, StateFailed, StateAllocating, StateReady)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	expired := []Session{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		expired = append(expired, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].ID < expired[j].ID })
	return expired, nil
}

func (s *PG) get(ctx context.Context, db pg.DB, id string, forUpdate bool) (Session, error) {
	return s.getBy(ctx, db, "id", id, forUpdate)
}

// getBy is get() on a different unique column.
//
// `column` is a code constant, never user input - it is interpolated into the query
// because the column name cannot be a placeholder. Both callers pass a literal.
func (s *PG) getBy(ctx context.Context, db pg.DB, column, value string, forUpdate bool) (Session, error) {
	query := `SELECT id, match_id, state, queue, region, map,
	                 coalesce(allocation_id, ''), coalesce(provider, ''), coalesce(address, ''),
	                 coalesce(port, 0), coalesce(result_id, ''), coalesce(runtime_token, ''), created_at, ready_at, finished_at,
	                 release_pending, release_attempts, allocated_at, released_at
	            FROM sessions WHERE ` + column + ` = $1`
	if forUpdate {
		query += ` FOR UPDATE`
	}

	var (
		session   Session
		createdAt time.Time
	)
	err := db.QueryRow(ctx, query, value).Scan(
		&session.ID, &session.MatchID, &session.State, &session.Queue, &session.Region, &session.Map,
		&session.AllocationID, &session.Provider, &session.Address, &session.Port, &session.ResultID,
		&session.RuntimeToken, &createdAt, &session.ReadyAt, &session.FinishedAt,
		&session.ReleasePending, &session.ReleaseAttempts,
		&session.AllocatedAt, &session.ReleasedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		if column == "match_id" {
			return Session{}, apierr.NotFound("no session for this match").WithDetail("match_id", value)
		}
		return Session{}, apierr.NotFound("session not found").WithDetail("session_id", value)
	}
	if err != nil {
		return Session{}, err
	}
	session.CreatedAt = createdAt.UTC()

	rows, err := db.Query(ctx,
		`SELECT player_id FROM session_players WHERE session_id = $1`, session.ID)
	if err != nil {
		return Session{}, err
	}
	defer rows.Close()

	session.Players = []string{}
	for rows.Next() {
		var playerID string
		if err := rows.Scan(&playerID); err != nil {
			return Session{}, err
		}
		session.Players = append(session.Players, playerID)
	}
	return session, rows.Err()
}

type scanner interface {
	Scan(dest ...any) error
}

func scanSession(row scanner) (Session, error) {
	var (
		session   Session
		createdAt time.Time
	)
	if err := row.Scan(
		&session.ID, &session.MatchID, &session.State, &session.Queue, &session.Region, &session.Map,
		&session.AllocationID, &session.Provider, &session.Address, &session.Port, &session.ResultID,
		&session.RuntimeToken, &createdAt, &session.ReadyAt, &session.FinishedAt,
		&session.ReleasePending, &session.ReleaseAttempts,
		&session.AllocatedAt, &session.ReleasedAt); err != nil {
		return Session{}, err
	}
	session.CreatedAt = createdAt.UTC()
	session.Players = []string{}
	return session, nil
}

func nullify(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nullifyPort(port int) *int {
	if port == 0 {
		return nil
	}
	return &port
}

var (
	_ Expirer          = (*PG)(nil)
	_ ReleaseTracker   = (*PG)(nil)
	_ AllocationLedger = (*PG)(nil)
)

// RecordResult implements ResultRecorder.
//
// match_results is the report header (one row per result_id, unique by session);
// match_player_results is the per-player detail. Both writes are idempotent: a
// retried report is a no-op, not a duplicate audit trail.
func (s *PG) RecordResult(ctx context.Context, match FinishedMatch) error {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	finishedAt := match.FinishedAt
	if finishedAt.IsZero() {
		finishedAt = time.Now().UTC()
	}

	return pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO match_results (result_id, session_id, duration_seconds, accepted_at)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (result_id) DO NOTHING`,
			match.ResultID, match.SessionID, match.DurationSeconds, finishedAt); err != nil {
			if pg.IsForeignKeyViolation(err) {
				return apierr.Invalid("unknown session: a report must belong to an existing session").
					WithDetail("session_id", match.SessionID)
			}
			return err
		}

		for _, player := range match.Players {
			if _, err := tx.Exec(ctx,
				`INSERT INTO match_player_results
				     (result_id, player_id, won, score, kills, deaths, damage)
				 VALUES ($1, $2, $3, $4, $5, $6, $7)
				 ON CONFLICT (result_id, player_id) DO NOTHING`,
				match.ResultID, player.PlayerID, player.Won, player.Score,
				player.Kills, player.Deaths, player.Damage); err != nil {
				return err
			}
		}
		return nil
	})
}

var _ ResultRecorder = (*PG)(nil)

// Allocations implements AllocationLedger.
func (s *PG) Allocations(ctx context.Context, q AllocationQuery) ([]Session, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	query := `SELECT id, match_id, state, queue, region, map,
	                 coalesce(allocation_id, ''), coalesce(provider, ''), coalesce(address, ''),
	                 coalesce(port, 0), coalesce(result_id, ''), coalesce(runtime_token, ''), created_at, ready_at, finished_at,
	                 release_pending, release_attempts, allocated_at, released_at
	            FROM sessions WHERE allocated_at IS NOT NULL`
	args := []any{}
	if q.UnreleasedOnly {
		query += ` AND released_at IS NULL`
	}
	if !q.Since.IsZero() {
		args = append(args, q.Since)
		query += ` AND allocated_at >= $` + strconv.Itoa(len(args))
	}
	query += ` ORDER BY allocated_at DESC, id`
	if q.Limit > 0 {
		args = append(args, q.Limit)
		query += ` LIMIT $` + strconv.Itoa(len(args))
	}

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Session{}
	for rows.Next() {
		session, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, session)
	}
	return out, rows.Err()
}
