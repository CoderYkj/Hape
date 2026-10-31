package account

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"gamebackend/pkg/apierr"
)

// Player is the backend's own identity record. Nakama owns credentials and
// sessions; this record owns everything the game itself needs to know.
type Player struct {
	ID          string    `json:"id" doc:"Backend player id, stable for the account lifetime."`
	ExternalID  string    `json:"external_id" doc:"Nakama user id that authenticated this player."`
	DisplayName string    `json:"display_name" doc:"Player-visible name."`
	CreatedAt   time.Time `json:"created_at" doc:"RFC3339 creation time."`
	UpdatedAt   time.Time `json:"updated_at" doc:"RFC3339 time of the last profile change."`
}

// Store is the persistence seam for the account service.
//
// v0 ships the memory implementation so the whole platform runs offline. The
// SQL implementation replaces it without touching handlers, because handlers
// only ever see this interface.
type Store interface {
	FindOrCreate(ctx context.Context, externalID, displayName string) (player Player, created bool, err error)
	GetByID(ctx context.Context, id string) (Player, error)
	UpdateDisplayName(ctx context.Context, id, displayName string) (Player, error)
}

// Memory is the in-memory Store used by tests and local dev.
type Memory struct {
	mu         sync.RWMutex
	byID       map[string]Player
	byExternal map[string]string
	serial     int64
	now        func() time.Time
}

// NewMemory builds an empty store.
func NewMemory() *Memory {
	return &Memory{byID: map[string]Player{}, byExternal: map[string]string{}, now: time.Now}
}

func (m *Memory) FindOrCreate(_ context.Context, externalID, displayName string) (Player, bool, error) {
	// Same guard as the SQL store: an empty external id would otherwise create an
	// unauthenticatable account. Kept identical so the two stores agree.
	if strings.TrimSpace(externalID) == "" {
		return Player{}, false, apierr.Invalid("external_id is required").WithDetail("field", "external_id")
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if id, ok := m.byExternal[externalID]; ok {
		return m.byID[id], false, nil
	}
	m.serial++
	player := Player{
		ID:          "p_" + strconv.FormatInt(m.serial, 36),
		ExternalID:  externalID,
		DisplayName: displayName,
		CreatedAt:   m.now().UTC(),
		UpdatedAt:   m.now().UTC(),
	}
	if player.DisplayName == "" {
		player.DisplayName = "player-" + player.ID
	}
	m.byID[player.ID] = player
	m.byExternal[externalID] = player.ID
	return player, true, nil
}

func (m *Memory) GetByID(_ context.Context, id string) (Player, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	player, ok := m.byID[id]
	if !ok {
		return Player{}, apierr.NotFound("player not found").WithDetail("player_id", id)
	}
	return player, nil
}

func (m *Memory) UpdateDisplayName(_ context.Context, id, displayName string) (Player, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	player, ok := m.byID[id]
	if !ok {
		return Player{}, apierr.NotFound("player not found").WithDetail("player_id", id)
	}
	player.DisplayName = displayName
	player.UpdatedAt = m.now().UTC()
	m.byID[id] = player
	return player, nil
}

// ValidateDisplayName enforces the shared naming rule. One rule, one function:
// a second copy of this logic is how two services end up disagreeing.
func ValidateDisplayName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", apierr.Invalid("display_name is required").WithDetail("field", "display_name")
	}
	if utf8.RuneCountInString(name) > 24 {
		return "", apierr.Invalid("display_name must be at most 24 characters").WithDetail("field", "display_name")
	}
	return name, nil
}
