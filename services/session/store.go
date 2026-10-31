package session

import (
	"sync"
	"time"

	"gamebackend/pkg/apierr"
)

// Session states. The transition graph is enforced by CanTransition; nothing
// else in the codebase may invent a new state.
const (
	StateAllocating = "allocating"
	StateReady      = "ready"
	StateInProgress = "in_progress"
	StateFinished   = "finished"
	StateFailed     = "failed"
)

// Session is one authoritative match instance and its lifecycle.
type Session struct {
	ID           string     `json:"id" doc:"Session id."`
	MatchID      string     `json:"match_id" doc:"Match id produced by matchmaking."`
	State        string     `json:"state" doc:"allocating, ready, in_progress, finished or failed."`
	Players      []string   `json:"players" doc:"Player ids expected in this match."`
	Queue        string     `json:"queue" doc:"Queue the match came from."`
	Region       string     `json:"region" doc:"Requested region."`
	Map          string     `json:"map" doc:"Map identifier passed to the dedicated server."`
	AllocationID string     `json:"allocation_id" doc:"Provider allocation id, used to release the server."`
	Provider     string     `json:"provider" doc:"local, gamelift, edgegap or agones."`
	Address      string     `json:"address" doc:"Dedicated server address reported by the provider."`
	Port         int        `json:"port" doc:"Dedicated server port."`
	ResultID     string     `json:"result_id,omitempty" doc:"Idempotency id of the accepted battle report."`
	CreatedAt    time.Time  `json:"created_at" doc:"RFC3339 creation time."`
	ReadyAt      *time.Time `json:"ready_at,omitempty" doc:"When the dedicated server reported ready."`
	FinishedAt   *time.Time `json:"finished_at,omitempty" doc:"When the accepted battle report landed."`
}

// PlayerResult is one player's authoritative outcome, reported by the server.
type PlayerResult struct {
	PlayerID string `json:"player_id" doc:"Player id."`
	Won      bool   `json:"won" doc:"True when the player was on the winning side."`
	Score    int64  `json:"score" doc:"Game-specific score, 0 or more."`
	Kills    int64  `json:"kills" doc:"Kills, 0 or more."`
	Deaths   int64  `json:"deaths" doc:"Deaths, 0 or more."`
	Damage   int64  `json:"damage" doc:"Damage dealt, 0 or more."`
}

// ErrInvalidTransition is returned when a state change is not allowed.
var ErrInvalidTransition = apierr.Precondition("invalid session state transition")

// Store is the persistence seam for sessions.
type Store interface {
	Create(session Session) (Session, error)
	Get(id string) (Session, error)
	// Update applies mutate under a lock and persists the result. Returning an
	// error from mutate aborts the write, which keeps the state machine atomic.
	Update(id string, mutate func(*Session) error) (Session, error)
}

// Memory is the in-memory Store.
type Memory struct {
	mu   sync.Mutex
	byID map[string]Session
}

// NewMemory builds an empty session store.
func NewMemory() *Memory { return &Memory{byID: map[string]Session{}} }

func (m *Memory) Create(session Session) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byID[session.ID]; exists {
		return Session{}, apierr.Conflict("session already exists").WithDetail("session_id", session.ID)
	}
	m.byID[session.ID] = session
	return session, nil
}

func (m *Memory) Get(id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.byID[id]
	if !ok {
		return Session{}, apierr.NotFound("session not found").WithDetail("session_id", id)
	}
	return session, nil
}

func (m *Memory) Update(id string, mutate func(*Session) error) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.byID[id]
	if !ok {
		return Session{}, apierr.NotFound("session not found").WithDetail("session_id", id)
	}
	if err := mutate(&session); err != nil {
		return Session{}, err
	}
	m.byID[id] = session
	return session, nil
}

// CanTransition reports whether from -> to is allowed.
//
//	allocating -> ready      (dedicated server reported in)
//	allocating -> failed     (allocation failed)
//	ready      -> in_progress (first player joined; set by the DS or a timer)
//	ready      -> finished   (short match, report lands without a join event)
//	in_progress-> finished   (battle report accepted)
//	any        -> failed     (provider lost the instance)
func CanTransition(from, to string) bool {
	if from == to {
		return true // idempotent re-report of the same state
	}
	switch from {
	case StateAllocating:
		return to == StateReady || to == StateFailed
	case StateReady:
		return to == StateInProgress || to == StateFinished || to == StateFailed
	case StateInProgress:
		return to == StateFinished || to == StateFailed
	default:
		return false
	}
}
