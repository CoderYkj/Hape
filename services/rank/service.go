// Package rank owns MMR and leaderboards.
//
// Boundary: it consumes authoritative match outcomes, never client claims, and
// it is the source of truth matchmaking reads when it snapshots a ticket's MMR.
package rank

import (
	"context"
	"log/slog"
	"net/http"

	"gamebackend/pkg/api"
	"gamebackend/pkg/apierr"
	"gamebackend/pkg/httpx"
)

// Service is the rank domain service.
type Service struct {
	log   *slog.Logger
	store Store
}

// New builds the service with the in-memory store.
func New(log *slog.Logger) *Service { return &Service{log: log, store: NewMemory()} }

// NewWithStore builds the service on a caller-supplied store.
func NewWithStore(log *slog.Logger, store Store) *Service {
	return &Service{log: log, store: store}
}

// Rating returns a player's current rating on the default board. The
// matchmaking service receives this as a lookup function so it never imports
// this package directly.
func (s *Service) Rating(ctx context.Context, playerID string) (int64, error) {
	return s.store.RatingOf(ctx, DefaultBoard, playerID)
}

// GetLeaderboardResponse is the leaderboard read model.
type GetLeaderboardResponse struct {
	Leaderboard Leaderboard `json:"leaderboard"`
}

// SubmitResultRequest is the body of
// POST /v1/rank/leaderboards/{board}/scores.
type SubmitResultRequest struct {
	PlayerID string `json:"player_id" doc:"Player whose rating changes."`
	Won      bool   `json:"won" doc:"Outcome of the match for this player."`
}

// SubmitResultResponse reports the new rating.
type SubmitResultResponse struct {
	Rating  Rating `json:"rating"`
	Delta   int64  `json:"delta" doc:"Rating change applied; 0 on an idempotent replay."`
	Applied bool   `json:"applied" doc:"false when the idempotency key was already used."`
}

// Register mounts the rank operations on r.
func (s *Service) Register(r *httpx.Router) {
	r.Handle(api.Op{
		ID:       "rank.get_leaderboard",
		Service:  "rank",
		Method:   http.MethodGet,
		Path:     "/v1/rank/leaderboards/{board}",
		Summary:  "Read a leaderboard",
		Response: api.T(GetLeaderboardResponse{}),
		Auth:     api.AuthPlayer,
	}, s.getLeaderboard)

	r.Handle(api.Op{
		ID:         "rank.submit_result",
		Service:    "rank",
		Method:     http.MethodPost,
		Path:       "/v1/rank/leaderboards/{board}/scores",
		Summary:    "Apply an authoritative match outcome to a player's rating",
		Request:    api.Body(SubmitResultRequest{}),
		Response:   api.T(SubmitResultResponse{}),
		Auth:       api.AuthService,
		Idempotent: true,
		Note:       "Requires an Idempotency-Key header; replay returns delta=0 and applied=false.",
	}, s.submitResult)
}

func (s *Service) getLeaderboard(r *http.Request) (any, error) {
	if _, err := httpx.RequirePlayer(r); err != nil {
		return nil, err
	}
	limit := 50
	board, err := s.store.Top(r.Context(), httpx.Param(r, "board"), limit)
	if err != nil {
		return nil, err
	}
	return GetLeaderboardResponse{Leaderboard: board}, nil
}

func (s *Service) submitResult(r *http.Request) (any, error) {
	if _, err := httpx.RequireService(r); err != nil {
		return nil, err
	}
	key, err := httpx.IdempotencyKey(r)
	if err != nil {
		return nil, err
	}
	var req SubmitResultRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	if req.PlayerID == "" {
		return nil, apierr.Invalid("player_id is required").WithDetail("field", "player_id")
	}
	board := httpx.Param(r, "board")
	rating, delta, applied, err := s.store.Apply(r.Context(), board, req.PlayerID, key, req.Won)
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(r.Context(), "rating updated", "board", board, "player_id", req.PlayerID,
		"delta", delta, "rating", rating.Rating, "applied", applied)
	return SubmitResultResponse{Rating: rating, Delta: delta, Applied: applied}, nil
}

// Apply updates a rating from an authoritative outcome. Shared by the HTTP
// handler and the in-process match fan-out.
func (s *Service) Apply(ctx context.Context, board, playerID, idempotencyKey string, won bool) (Rating, int64, bool, error) {
	return s.store.Apply(ctx, board, playerID, idempotencyKey, won)
}
