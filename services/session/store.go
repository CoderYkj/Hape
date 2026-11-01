package session

import (
	"context"
	"sort"
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
	ID           string   `json:"id" doc:"Session id."`
	MatchID      string   `json:"match_id" doc:"Match id produced by matchmaking."`
	State        string   `json:"state" doc:"allocating, ready, in_progress, finished or failed."`
	Players      []string `json:"players" doc:"Player ids expected in this match."`
	Queue        string   `json:"queue" doc:"Queue the match came from."`
	Region       string   `json:"region" doc:"Requested region."`
	Map          string   `json:"map" doc:"Map identifier passed to the dedicated server."`
	AllocationID string   `json:"allocation_id" doc:"Provider allocation id, used to release the server."`
	Provider     string   `json:"provider" doc:"local, gamelift, edgegap or agones."`
	Address      string   `json:"address" doc:"Dedicated server address reported by the provider."`
	Port         int      `json:"port" doc:"Dedicated server port."`
	ResultID     string   `json:"result_id,omitempty" doc:"Idempotency id of the accepted battle report."`
	// RuntimeToken is the per-session credential the dedicated server must present
	// on mark_ready/submit_result. It is stripped from every read path: only the
	// create response (service-only op) carries it, because a player reading the
	// session must never receive the report credential.
	RuntimeToken string     `json:"runtime_token,omitempty" doc:"Present only in the session.create response."`
	CreatedAt    time.Time  `json:"created_at" doc:"RFC3339 creation time."`
	ReadyAt      *time.Time `json:"ready_at,omitempty" doc:"When the dedicated server reported ready."`
	FinishedAt   *time.Time `json:"finished_at,omitempty" doc:"When the accepted battle report landed."`

	// ReleasePending records that the dedicated server could not be confirmed gone.
	// The reconciler retries; nothing player-facing reads it, so it never reaches the
	// wire (json:"-" keeps it out of the published contract too).
	ReleasePending bool `json:"-" doc:"Internal: the server release is being retried."`
	// ReleaseAttempts counts the failed attempts, so "still pending after 20 tries" is
	// visible instead of looking identical to "just failed once".
	ReleaseAttempts int `json:"-" doc:"Internal: failed release attempts."`
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
	Create(ctx context.Context, session Session) (Session, error)
	Get(ctx context.Context, id string) (Session, error)
	// GetByMatch resolves the session allocated for a match. A client learns its
	// match id from the matchmaking ticket and nothing else, so without this a
	// client can be in a match it has no way to find the server for.
	GetByMatch(ctx context.Context, matchID string) (Session, error)
	// Update applies mutate under a lock and persists the result. Returning an
	// error from mutate aborts the write, which keeps the state machine atomic.
	Update(ctx context.Context, id string, mutate func(*Session) error) (Session, error)
}

// RecordResult implements ResultRecorder. The memory store keeps the report in a
// map: process-local, forgotten on restart, which is exactly why the SQL store
// exists.
func (m *Memory) RecordResult(_ context.Context, match FinishedMatch) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.results[match.ResultID]; exists {
		return nil
	}
	m.results[match.ResultID] = match
	return nil
}

// Expirer is the optional capability the reconciler needs: find sessions that
// have been sitting in a pre-match state for too long and mark them failed.
//
// It is deliberately not part of Store. A store that cannot scan (sharded, or a
// remote index behind its own API) simply does not implement it, and the caller
// can report "this deployment cannot sweep" instead of pretending it ran.
type Expirer interface {
	// ExpireStale returns the sessions it failed, or an error. The error matters:
	// a sweep that silently returns nothing looks identical to "nothing to do".
	ExpireStale(ctx context.Context, before time.Time) ([]Session, error)
}

// ReleaseTracker is the optional store capability the reconciler needs: find sessions
// whose dedicated server was never confirmed gone.
//
// Separate from Store for the same reason Expirer is: a store that cannot scan simply
// does not implement it, and the caller reports that instead of pretending it swept.
type ReleaseTracker interface {
	PendingReleases(ctx context.Context, limit int) ([]Session, error)
}

// Memory is the in-memory Store.
type Memory struct {
	mu      sync.Mutex
	byID    map[string]Session
	byMatch map[string]string
	results map[string]FinishedMatch
}

// NewMemory builds an empty session store.
func NewMemory() *Memory {
	return &Memory{byID: map[string]Session{}, byMatch: map[string]string{}, results: map[string]FinishedMatch{}}
}

func (m *Memory) Create(_ context.Context, session Session) (Session, error) {
	// A brand new session has nothing to release yet; both stores agree on that, and
	// the SQL store relies on the column defaults.
	session.ReleasePending = false
	session.ReleaseAttempts = 0
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byID[session.ID]; exists {
		return Session{}, apierr.Conflict("session already exists").WithDetail("session_id", session.ID)
	}
	// Mirrors sessions_one_per_match: one session per match, so a retried create
	// cannot allocate a second dedicated server.
	if existingID, ok := m.byMatch[session.MatchID]; ok && existingID != session.ID {
		return Session{}, apierr.Conflict("a session already exists for this match").
			WithDetail("match_id", session.MatchID)
	}
	m.byID[session.ID] = session
	m.byMatch[session.MatchID] = session.ID
	return session, nil
}

func (m *Memory) Get(_ context.Context, id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.byID[id]
	if !ok {
		return Session{}, apierr.NotFound("session not found").WithDetail("session_id", id)
	}
	return session, nil
}

// GetByMatch implements Store.
func (m *Memory) GetByMatch(_ context.Context, matchID string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byMatch[matchID]
	if !ok {
		return Session{}, apierr.NotFound("no session for this match").WithDetail("match_id", matchID)
	}
	return m.byID[id], nil
}

func (m *Memory) Update(_ context.Context, id string, mutate func(*Session) error) (Session, error) {
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

// PendingReleases implements ReleaseTracker.
func (m *Memory) PendingReleases(_ context.Context, limit int) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pending := []Session{}
	for _, session := range m.byID {
		if session.ReleasePending {
			pending = append(pending, session)
		}
	}
	// Oldest first, and deterministic: a sweep that reorders itself on every tick is
	// impossible to diff in a log.
	sort.Slice(pending, func(i, j int) bool { return pending[i].CreatedAt.Before(pending[j].CreatedAt) })
	if limit > 0 && len(pending) > limit {
		pending = pending[:limit]
	}
	return pending, nil
}

// ExpireStale fails every session still allocating or ready since before
// `before`, and returns them. in_progress is excluded on purpose: a legitimate
// long match and an abandoned one are indistinguishable without a per-game
// timeout, and killing a live match is worse than paying for a leaked one.
func (m *Memory) ExpireStale(_ context.Context, before time.Time) ([]Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var expired []Session
	for id, session := range m.byID {
		if session.State != StateAllocating && session.State != StateReady {
			continue
		}
		if !session.CreatedAt.Before(before) {
			continue
		}
		if !CanTransition(session.State, StateFailed) {
			continue
		}
		session.State = StateFailed
		m.byID[id] = session
		expired = append(expired, session)
	}
	// Deterministic order: map iteration is random, and a reconciler log that
	// reorders itself on every tick is useless for diffing.
	sort.Slice(expired, func(i, j int) bool { return expired[i].ID < expired[j].ID })
	return expired, nil
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
