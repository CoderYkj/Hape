// Package matchmaking turns queue tickets into matches.
//
// Boundary: it decides *who plays together*, never *where*. Allocation and
// session lifecycle belong to the session service.
package matchmaking

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"gamebackend/pkg/api"
	"gamebackend/pkg/apierr"
	"gamebackend/pkg/httpx"
)

// DefaultMMRBand is how far apart two players may be to share a match.
const DefaultMMRBand int64 = 200

// RatingLookup returns a player's current rating. Injected by the platform so
// this package never imports the rank service directly.
type RatingLookup func(ctx context.Context, playerID string) (int64, error)

// Service is the matchmaking domain service.
type Service struct {
	log     *slog.Logger
	store   Store
	ratings RatingLookup
}

// New builds the service. ratings may be nil (everyone starts at 1000).
func New(log *slog.Logger, ratings RatingLookup) *Service {
	if ratings == nil {
		ratings = func(context.Context, string) (int64, error) { return 1000, nil }
	}
	return &Service{log: log, store: NewMemory(), ratings: ratings}
}

// NewWithStore builds the service on a caller-supplied store.
func NewWithStore(log *slog.Logger, store Store, ratings RatingLookup) *Service {
	if ratings == nil {
		ratings = func(context.Context, string) (int64, error) { return 1000, nil }
	}
	return &Service{log: log, store: store, ratings: ratings}
}

// CreateTicketRequest is the body of POST /v1/matchmaking/tickets.
type CreateTicketRequest struct {
	Queue string   `json:"queue" doc:"Queue name, e.g. ranked_1v1 or ranked_squad."`
	Party []string `json:"party,omitempty" doc:"Additional player ids queued with the caller (team members)."`
}

// CreateTicketResponse returns the queue ticket.
type CreateTicketResponse struct {
	Ticket Ticket `json:"ticket"`
}

// GetTicketResponse returns one ticket.
type GetTicketResponse struct {
	Ticket Ticket `json:"ticket"`
}

// PopRequest is the body of POST /v1/matchmaking/queues/{queue}/pop.
type PopRequest struct {
	Count int `json:"count" doc:"How many players the match needs, at most 10."`
}

// PopResponse is the result of a dequeue attempt.
type PopResponse struct {
	Matched bool     `json:"matched" doc:"false when the queue could not produce a full match."`
	MatchID string   `json:"match_id,omitempty" doc:"Match id shared by every returned ticket."`
	Tickets []Ticket `json:"tickets,omitempty" doc:"Tickets that were matched."`
}

// Register mounts the matchmaking operations on r.
func (s *Service) Register(r *httpx.Router) {
	r.Handle(api.Op{
		ID:         "matchmaking.create_ticket",
		Service:    "matchmaking",
		Method:     http.MethodPost,
		Path:       "/v1/matchmaking/tickets",
		Summary:    "Enqueue the caller (and party) for a match",
		Request:    api.Body(CreateTicketRequest{}),
		Response:   api.T(CreateTicketResponse{}),
		Auth:       api.AuthPlayer,
		Idempotent: true,
		Note:       "Idempotent by (queue, player): a second enqueue returns the existing waiting ticket.",
	}, s.createTicket)

	r.Handle(api.Op{
		ID:       "matchmaking.get_ticket",
		Service:  "matchmaking",
		Method:   http.MethodGet,
		Path:     "/v1/matchmaking/tickets/{ticket_id}",
		Summary:  "Poll one ticket for its match assignment",
		Response: api.T(GetTicketResponse{}),
		Auth:     api.AuthPlayer,
	}, s.getTicket)

	r.Handle(api.Op{
		ID:       "matchmaking.pop",
		Service:  "matchmaking",
		Method:   http.MethodPost,
		Path:     "/v1/matchmaking/queues/{queue}/pop",
		Summary:  "Server-side dequeue: form one match from the queue",
		Request:  api.Body(PopRequest{}),
		Response: api.T(PopResponse{}),
		Auth:     api.AuthService,
		Note:     "Not idempotent: each call removes tickets from the queue. Exactly one matchmaker loop may pop a given queue at a time; running two of them silently splits matches.",
	}, s.pop)
}

func (s *Service) createTicket(r *http.Request) (any, error) {
	playerID, err := httpx.RequirePlayer(r)
	if err != nil {
		return nil, err
	}
	var req CreateTicketRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	if req.Queue == "" {
		return nil, apierr.Invalid("queue is required").WithDetail("field", "queue")
	}
	existing, waiting, err := s.store.FindWaitingByPlayer(r.Context(), req.Queue, playerID)
	if err != nil {
		// Not "assume nothing is waiting": that would create a second ticket and
		// turn a database blip into a confusing 409 for the player.
		return nil, apierr.Unavailable("queue lookup failed, retry").WithCause(err)
	}
	if waiting {
		return CreateTicketResponse{Ticket: existing}, nil
	}

	mmr, err := s.ratings(r.Context(), playerID)
	if err != nil {
		// Enqueueing at a guessed MMR would poison match quality silently.
		return nil, apierr.Unavailable("rating lookup failed, retry").WithCause(err)
	}

	ticket, err := s.store.Create(r.Context(), Ticket{
		PlayerID: playerID,
		Party:    req.Party,
		Queue:    req.Queue,
		MMR:      mmr,
	})
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(r.Context(), "ticket created", "ticket_id", ticket.ID, "queue", ticket.Queue, "mmr", ticket.MMR)
	return CreateTicketResponse{Ticket: ticket}, nil
}

func (s *Service) getTicket(r *http.Request) (any, error) {
	playerID, err := httpx.RequirePlayer(r)
	if err != nil {
		return nil, err
	}
	ticket, err := s.store.Get(r.Context(), httpx.Param(r, "ticket_id"))
	if err != nil {
		return nil, err
	}
	if ticket.PlayerID != playerID {
		return nil, apierr.PermissionDenied("ticket belongs to another player")
	}
	return GetTicketResponse{Ticket: ticket}, nil
}

func (s *Service) pop(r *http.Request) (any, error) {
	if _, err := httpx.RequireService(r); err != nil {
		return nil, err
	}
	queue := httpx.Param(r, "queue")
	var req PopRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	if req.Count < 2 || req.Count > 10 {
		return nil, apierr.Invalid("count must be between 2 and 10").WithDetail("field", "count")
	}

	// Time-based, not a process-local counter: with a persistent store the same
	// (see spec 0003 section 6 - this exact bug was found by running twice)
	// queue would mint m_ranked_1v1_1 again after a restart, and sessions_one_per_match
	// would (correctly) refuse the second match. The counter is not a unique
	// identifier, it is a convenience.
	matchID := "m_" + queue + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	tickets, err := s.store.Pop(r.Context(), queue, req.Count, DefaultMMRBand, matchID)
	if err != nil {
		if errors.Is(err, ErrNoWaiting) {
			return PopResponse{Matched: false}, nil
		}
		return nil, err
	}
	s.log.InfoContext(r.Context(), "match formed", "match_id", matchID, "queue", queue, "players", len(tickets))
	return PopResponse{Matched: true, MatchID: matchID, Tickets: tickets}, nil
}
