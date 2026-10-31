package account

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
)

// queryTimeout bounds one account query. Accounts are read on every request, so
// a hung connection must turn into an error, not a queue.
const queryTimeout = 3 * time.Second

// PG is the PostgreSQL Store.
//
// players.external_id is UNIQUE, and that is the whole idempotency story of
// login: the second call does not find-or-create, it collides and the store
// returns the existing row.
type PG struct {
	pool *pgxpool.Pool
}

// NewPG builds the SQL-backed account store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// FindOrCreate implements Store.
func (s *PG) FindOrCreate(ctx context.Context, externalID, displayName string) (Player, bool, error) {
	if strings.TrimSpace(externalID) == "" {
		return Player{}, false, apierr.Invalid("external_id is required").WithDetail("field", "external_id")
	}

	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	id := newPlayerID()
	if displayName == "" {
		displayName = "player-" + id
	}

	// One implicit transaction: if this collides, the connection is fine to reuse
	// for the lookup below (unlike a failed statement inside an explicit tx).
	player, err := s.scanPlayer(s.pool.QueryRow(ctx,
		`INSERT INTO players (id, external_id, display_name) VALUES ($1, $2, $3)
		 RETURNING id, external_id, display_name, created_at, updated_at`,
		id, externalID, displayName))
	if err == nil {
		return player, true, nil
	}
	if !pg.IsUniqueViolation(err) {
		return Player{}, false, err
	}

	// Same Nakama user, second login: return the row that is already there.
	existing, err := s.scanPlayer(s.pool.QueryRow(ctx,
		`SELECT id, external_id, display_name, created_at, updated_at
		   FROM players WHERE external_id = $1`, externalID))
	if err != nil {
		return Player{}, false, err
	}
	return existing, false, nil
}

// GetByID implements Store.
func (s *PG) GetByID(ctx context.Context, id string) (Player, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	player, err := s.scanPlayer(s.pool.QueryRow(ctx,
		`SELECT id, external_id, display_name, created_at, updated_at FROM players WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Player{}, apierr.NotFound("player not found").WithDetail("player_id", id)
	}
	return player, err
}

// UpdateDisplayName implements Store.
func (s *PG) UpdateDisplayName(ctx context.Context, id, displayName string) (Player, error) {
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()

	player, err := s.scanPlayer(s.pool.QueryRow(ctx,
		`UPDATE players SET display_name = $2, updated_at = now()
		  WHERE id = $1
		 RETURNING id, external_id, display_name, created_at, updated_at`,
		id, displayName))
	if errors.Is(err, pgx.ErrNoRows) {
		return Player{}, apierr.NotFound("player not found").WithDetail("player_id", id)
	}
	return player, err
}

func (s *PG) scanPlayer(row pgx.Row) (Player, error) {
	var (
		player    Player
		createdAt time.Time
		updatedAt time.Time
	)
	if err := row.Scan(&player.ID, &player.ExternalID, &player.DisplayName, &createdAt, &updatedAt); err != nil {
		return Player{}, err
	}
	player.CreatedAt = createdAt.UTC()
	player.UpdatedAt = updatedAt.UTC()
	return player, nil
}

// newPlayerID mints an opaque player id. It is random rather than sequential on
// purpose: a sequential id leaks how many players the game has, and lets anyone
// guess a neighbour's id.
func newPlayerID() string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand failing is a broken machine; a timestamp keeps the id
		// unique enough for one process rather than panicking in a request path.
		return "p_" + hex.EncodeToString([]byte(time.Now().Format("150405")))[:10]
	}
	return "p_" + hex.EncodeToString(raw[:])
}
