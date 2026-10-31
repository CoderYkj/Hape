package team

import (
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
	Create(ownerID, name string) (Team, error)
	Get(id string) (Team, error)
	Join(teamID, playerID string) (team Team, applied bool, err error)
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

func (m *Memory) Create(ownerID, name string) (Team, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

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

func (m *Memory) Get(id string) (Team, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	team, ok := m.byID[id]
	if !ok {
		return Team{}, apierr.NotFound("team not found").WithDetail("team_id", id)
	}
	return team, nil
}

func (m *Memory) Join(teamID, playerID string) (Team, bool, error) {
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
	if len(team.Members) >= MaxTeamSize {
		return Team{}, false, apierr.Precondition("team is full").WithDetail("max_team_size", strconv.Itoa(MaxTeamSize))
	}
	team.Members = append(team.Members, Member{PlayerID: playerID, Role: "member", JoinedAt: m.now().UTC()})
	team.UpdatedAt = m.now().UTC()
	m.byID[teamID] = team
	return team, true, nil
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
