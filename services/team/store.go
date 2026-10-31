package team

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"gamebackend/pkg/apierr"
)

// MaxTeamSize is the squad size the matchmaking queue is built around.
const MaxTeamSize = 5

// Member is one team slot.
type Member struct {
	PlayerID string    `json:"player_id" doc:"Backend player id."`
	Role     string    `json:"role" doc:"owner or member."`
	JoinedAt time.Time `json:"joined_at" doc:"RFC3339 join time."`
}

// Team is a persistent group of players.
type Team struct {
	ID        string    `json:"id" doc:"Team id."`
	Name      string    `json:"name" doc:"Human-readable team name."`
	OwnerID   string    `json:"owner_id" doc:"Player id that created the team."`
	Members   []Member  `json:"members" doc:"Team members, owner first."`
	CreatedAt time.Time `json:"created_at" doc:"RFC3339 creation time."`
	UpdatedAt time.Time `json:"updated_at" doc:"RFC3339 time of the last membership change."`
}

// Store is the persistence seam for teams.
type Store interface {
	Create(ctx context.Context, ownerID, name string) (Team, error)
	Get(ctx context.Context, id string) (Team, error)
	Join(ctx context.Context, teamID, playerID string) (team Team, applied bool, err error)
}

// Memory is the in-memory Store.
type Memory struct {
	mu     sync.RWMutex
	byID   map[string]Team
	serial int64
	now    func() time.Time
}

// NewMemory builds an empty team store.
func NewMemory() *Memory { return &Memory{byID: map[string]Team{}, now: time.Now} }

func (m *Memory) Create(_ context.Context, ownerID, name string) (Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Same rule as the database's team_members_one_team_per_player index: a player
	// belongs to one team at a time. Kept identical in both stores so the
	// equivalence test is meaningful.
	if teamID, ok := m.teamOfLocked(ownerID); ok {
		return Team{}, apierr.Precondition("player is already in a team").WithDetail("team_id", teamID)
	}

	m.serial++
	now := m.now().UTC()
	team := Team{
		ID:        "t_" + strconv.FormatInt(m.serial, 36),
		Name:      name,
		OwnerID:   ownerID,
		Members:   []Member{{PlayerID: ownerID, Role: "owner", JoinedAt: now}},
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.byID[team.ID] = team
	return team, nil
}

func (m *Memory) Get(_ context.Context, id string) (Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	team, ok := m.byID[id]
	if !ok {
		return Team{}, apierr.NotFound("team not found").WithDetail("team_id", id)
	}
	return team, nil
}

func (m *Memory) Join(_ context.Context, teamID, playerID string) (Team, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	team, ok := m.byID[teamID]
	if !ok {
		return Team{}, false, apierr.NotFound("team not found").WithDetail("team_id", teamID)
	}
	for _, member := range team.Members {
		if member.PlayerID == playerID {
			return team, false, nil
		}
	}
	if otherID, ok := m.teamOfLocked(playerID); ok && otherID != teamID {
		return Team{}, false, apierr.Precondition("player is already in another team").WithDetail("team_id", otherID)
	}
	if len(team.Members) >= MaxTeamSize {
		return Team{}, false, apierr.Precondition("team is full").WithDetail("max_team_size", strconv.Itoa(MaxTeamSize))
	}
	team.Members = append(team.Members, Member{PlayerID: playerID, Role: "member", JoinedAt: m.now().UTC()})
	team.UpdatedAt = m.now().UTC()
	m.byID[teamID] = team
	return team, true, nil
}

// teamOfLocked returns the team a player currently belongs to, if any. Callers
// hold the mutex.
func (m *Memory) teamOfLocked(playerID string) (string, bool) {
	for teamID, team := range m.byID {
		for _, member := range team.Members {
			if member.PlayerID == playerID {
				return teamID, true
			}
		}
	}
	return "", false
}

// ValidateName enforces the team naming rule.
func ValidateName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", apierr.Invalid("name is required").WithDetail("field", "name")
	}
	if len([]rune(name)) > 32 {
		return "", apierr.Invalid("name must be at most 32 characters").WithDetail("field", "name")
	}
	return name, nil
}
