package team

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
// The database carries two rules the memory store has to mirror: one team per
// player (team_members_one_team_per_player) and exactly one owner per team
// (team_members_single_owner). Both are constraints, not application logic, so a
// bug cannot talk its way past them.
type PG struct {
	pool *pgxpool.Pool
}

// NewPG builds the SQL-backed team store.
func NewPG(pool *pgxpool.Pool) *PG { return &PG{pool: pool} }

// Create implements Store.
func (s *PG) Create(ownerID, name string) (Team, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	id := "t_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	now := time.Now().UTC()

	err := pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO teams (id, name, owner_id) VALUES ($1, $2, $3)`,
			id, name, ownerID); err != nil {
			if pg.IsForeignKeyViolation(err) {
				return apierr.Invalid("unknown owner: a team owner must be an existing player").
					WithDetail("player_id", ownerID)
			}
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO team_members (team_id, player_id, role) VALUES ($1, $2, 'owner')`,
			id, ownerID); err != nil {
			if pg.IsUniqueViolation(err) {
				return apierr.Precondition("player is already in a team").WithDetail("player_id", ownerID)
			}
			return err
		}
		return nil
	})
	if err != nil {
		return Team{}, err
	}
	return Team{
		ID:        id,
		Name:      name,
		OwnerID:   ownerID,
		Members:   []Member{{PlayerID: ownerID, Role: "owner", JoinedAt: now}},
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// Get implements Store.
func (s *PG) Get(id string) (Team, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()
	return s.get(ctx, s.pool, id)
}

// Join implements Store.
func (s *PG) Join(teamID, playerID string) (Team, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	var (
		result  Team
		applied bool
	)
	err := pg.InTx(ctx, s.pool, func(tx pgx.Tx) error {
		// Lock the team row so two joins cannot both see "4 members, room for one".
		var exists bool
		if err := tx.QueryRow(ctx,
			`SELECT true FROM teams WHERE id = $1 FOR UPDATE`, teamID).Scan(&exists); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return apierr.NotFound("team not found").WithDetail("team_id", teamID)
			}
			return err
		}

		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM team_members WHERE team_id = $1`, teamID).Scan(&count); err != nil {
			return err
		}

		tag, err := tx.Exec(ctx,
			`INSERT INTO team_members (team_id, player_id, role) VALUES ($1, $2, 'member')
			 ON CONFLICT (team_id, player_id) DO NOTHING`,
			teamID, playerID)
		if err != nil {
			if pg.IsForeignKeyViolation(err) {
				return apierr.Invalid("unknown player: a member must be an existing player").
					WithDetail("player_id", playerID)
			}
			if pg.IsUniqueViolation(err) {
				// The partial-unique index on player_id: one team per player.
				return apierr.Precondition("player is already in another team").WithDetail("player_id", playerID)
			}
			return err
		}

		if tag.RowsAffected() == 0 {
			// Already a member: idempotent no-op, return the team as it stands.
			current, err := s.get(ctx, tx, teamID)
			if err != nil {
				return err
			}
			result, applied = current, false
			return nil
		}

		if count >= MaxTeamSize {
			// The insert already happened inside this transaction; rolling back
			// undoes it, which is why the capacity check can sit here.
			return apierr.Precondition("team is full").
				WithDetail("max_team_size", strconv.Itoa(MaxTeamSize))
		}

		if _, err := tx.Exec(ctx,
			`UPDATE teams SET updated_at = now() WHERE id = $1`, teamID); err != nil {
			return err
		}

		current, err := s.get(ctx, tx, teamID)
		if err != nil {
			return err
		}
		result, applied = current, true
		return nil
	})
	if err != nil {
		return Team{}, false, err
	}
	return result, applied, nil
}

func (s *PG) get(ctx context.Context, db pg.DB, id string) (Team, error) {
	var (
		team      Team
		createdAt time.Time
		updatedAt time.Time
	)
	err := db.QueryRow(ctx,
		`SELECT id, name, owner_id, created_at, updated_at FROM teams WHERE id = $1`, id).
		Scan(&team.ID, &team.Name, &team.OwnerID, &createdAt, &updatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Team{}, apierr.NotFound("team not found").WithDetail("team_id", id)
	}
	if err != nil {
		return Team{}, err
	}
	team.CreatedAt, team.UpdatedAt = createdAt.UTC(), updatedAt.UTC()

	rows, err := db.Query(ctx,
		`SELECT player_id, role, joined_at FROM team_members WHERE team_id = $1`, id)
	if err != nil {
		return Team{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var member Member
		if err := rows.Scan(&member.PlayerID, &member.Role, &member.JoinedAt); err != nil {
			return Team{}, err
		}
		member.JoinedAt = member.JoinedAt.UTC()
		team.Members = append(team.Members, member)
	}
	if err := rows.Err(); err != nil {
		return Team{}, err
	}
	// Owner first, then join order: the contract says "owner first", and a
	// nil-safe sort keeps the wire format stable regardless of row order.
	sort.SliceStable(team.Members, func(i, j int) bool {
		if (team.Members[i].Role == "owner") != (team.Members[j].Role == "owner") {
			return team.Members[i].Role == "owner"
		}
		return team.Members[i].JoinedAt.Before(team.Members[j].JoinedAt)
	})
	if team.Members == nil {
		team.Members = []Member{}
	}
	return team, nil
}
