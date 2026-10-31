package matchmaking

import (
	"context"
	"strconv"
	"sync"
	"time"

	"gamebackend/pkg/apierr"
)

// Ticket lifecycle states.
const (
	StatusWaiting = "waiting"
	StatusMatched = "matched"
)

// Ticket is one player's (or party's) queue entry.
type Ticket struct {
	ID        string    `json:"id" doc:"Ticket id."`
	PlayerID  string    `json:"player_id" doc:"Player that owns the ticket."`
	Party     []string  `json:"party" doc:"Additional player ids queued together."`
	Queue     string    `json:"queue" doc:"Queue name, e.g. ranked_1v1."`
	MMR       int64     `json:"mmr" doc:"Rating snapshot taken at enqueue time; never client-supplied."`
	Status    string    `json:"status" doc:"waiting or matched."`
	MatchID   string    `json:"match_id,omitempty" doc:"Match id once the ticket has been popped."`
	CreatedAt time.Time `json:"created_at" doc:"RFC3339 enqueue time."`
	UpdatedAt time.Time `json:"updated_at" doc:"RFC3339 time of the last state change."`
}

// Store is the persistence seam for the queue.
type Store interface {
	Create(ctx context.Context, ticket Ticket) (Ticket, error)
	Get(ctx context.Context, id string) (Ticket, error)
	// FindWaitingByPlayer returns the caller's waiting ticket in a queue, or
	// ErrNoWaiting. It makes enqueue idempotent without an idempotency key.
	FindWaitingByPlayer(ctx context.Context, queue, playerID string) (Ticket, bool, error)
	Pop(ctx context.Context, queue string, count int, band int64, matchID string) ([]Ticket, error)
	Waiting(ctx context.Context, queue string) int
}

// ErrNoWaiting is returned when the queue holds no matchable tickets.
var ErrNoWaiting = apierr.New(apierr.CodeNotFound, "no matchable tickets in queue")

// Memory is the in-memory Store.
type Memory struct {
	mu      sync.Mutex
	byID    map[string]Ticket
	waiting map[string][]string
	serial  int64
	now     func() time.Time
}

// NewMemory builds an empty queue store.
func NewMemory() *Memory {
	return &Memory{byID: map[string]Ticket{}, waiting: map[string][]string{}, now: time.Now}
}

func (m *Memory) Create(_ context.Context, ticket Ticket) (Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Mirrors the partial unique index matchmaking_one_waiting_ticket: one waiting
	// ticket per (queue, player). The service checks first, but the database is the
	// guard that survives a race, so both stores enforce it.
	for _, id := range m.waiting[ticket.Queue] {
		if m.byID[id].PlayerID == ticket.PlayerID {
			return Ticket{}, apierr.Conflict("player already has a waiting ticket in this queue").
				WithDetail("queue", ticket.Queue)
		}
	}

	m.serial++
	ticket.ID = "q_" + strconv.FormatInt(m.serial, 36)
	ticket.Status = StatusWaiting
	ticket.CreatedAt = m.now().UTC()
	ticket.UpdatedAt = ticket.CreatedAt
	m.byID[ticket.ID] = ticket
	m.waiting[ticket.Queue] = append(m.waiting[ticket.Queue], ticket.ID)
	return ticket, nil
}

func (m *Memory) Get(_ context.Context, id string) (Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ticket, ok := m.byID[id]
	if !ok {
		return Ticket{}, apierr.NotFound("ticket not found").WithDetail("ticket_id", id)
	}
	return ticket, nil
}

func (m *Memory) FindWaitingByPlayer(_ context.Context, queue, playerID string) (Ticket, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range m.waiting[queue] {
		if ticket := m.byID[id]; ticket.PlayerID == playerID {
			return ticket, true, nil
		}
	}
	return Ticket{}, false, nil
}

func (m *Memory) Waiting(_ context.Context, queue string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.waiting[queue])
}

func (m *Memory) Pop(_ context.Context, queue string, count int, band int64, matchID string) ([]Ticket, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	ids := m.waiting[queue]
	if len(ids) == 0 {
		return nil, ErrNoWaiting
	}
	anchor := m.byID[ids[0]]

	chosen := make([]Ticket, 0, count)
	kept := make([]string, 0, len(ids))
	for _, id := range ids {
		ticket := m.byID[id]
		if len(chosen) < count && abs64(ticket.MMR-anchor.MMR) <= band {
			chosen = append(chosen, ticket)
			continue
		}
		kept = append(kept, id)
	}
	m.waiting[queue] = kept

	// A one-player "match" is worse than waiting: keep the queue as it was.
	if len(chosen) < 2 {
		m.waiting[queue] = ids
		return nil, ErrNoWaiting
	}

	now := m.now().UTC()
	for i := range chosen {
		chosen[i].Status = StatusMatched
		chosen[i].MatchID = matchID
		chosen[i].UpdatedAt = now
		m.byID[chosen[i].ID] = chosen[i]
	}
	return chosen, nil
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
