package progress

import (
	"sync"
	"time"

	"gamebackend/pkg/apierr"
)

// XPPerLevel is the progression curve. One constant, one place: a second copy
// is how the client and the server disagree about what level means.
const XPPerLevel int64 = 1000

// Progress is a player's progression record.
type Progress struct {
	PlayerID  string    `json:"player_id" doc:"Player id."`
	Level     int32     `json:"level" doc:"Derived from total_xp; starts at 1."`
	TotalXP   int64     `json:"total_xp" doc:"Lifetime experience, never decreases."`
	Unlocks   []string  `json:"unlocks" doc:"Content ids unlocked so far."`
	UpdatedAt time.Time `json:"updated_at" doc:"RFC3339 time of the last change."`
}

// Store is the persistence seam for progression.
type Store interface {
	Get(playerID string) (Progress, error)
	GrantXP(playerID, idempotencyKey string, xp int64, reason string) (Progress, bool, error)
}

// Memory is the in-memory Store.
type Memory struct {
	mu       sync.Mutex
	byPlayer map[string]Progress
	applied  map[string]Progress
	now      func() time.Time
}

// NewMemory builds an empty progression store.
func NewMemory() *Memory {
	return &Memory{byPlayer: map[string]Progress{}, applied: map[string]Progress{}, now: time.Now}
}

func (m *Memory) Get(playerID string) (Progress, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if player, ok := m.byPlayer[playerID]; ok {
		return player, nil
	}
	// A player with no history is not an error: level 1, zero XP.
	return Progress{PlayerID: playerID, Level: 1, Unlocks: []string{}}, nil
}

func (m *Memory) GrantXP(playerID, idempotencyKey string, xp int64, reason string) (Progress, bool, error) {
	if idempotencyKey == "" {
		return Progress{}, false, apierr.Invalid("idempotency key is required")
	}
	if xp <= 0 {
		return Progress{}, false, apierr.Invalid("xp must be greater than zero").WithDetail("field", "xp")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if previous, ok := m.applied[playerID+"|"+idempotencyKey]; ok {
		return previous, false, nil
	}
	player := m.byPlayer[playerID]
	player.PlayerID = playerID
	player.TotalXP += xp
	player.Level = levelFor(player.TotalXP)
	if player.Unlocks == nil {
		player.Unlocks = []string{}
	}
	player.UpdatedAt = m.now().UTC()

	m.byPlayer[playerID] = player
	m.applied[playerID+"|"+idempotencyKey] = player
	return player, true, nil
}

func levelFor(totalXP int64) int32 {
	if totalXP < 0 {
		return 1
	}
	return int32(totalXP/XPPerLevel) + 1
}
