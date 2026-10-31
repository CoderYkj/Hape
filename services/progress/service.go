// Package progress owns player progression: experience, level, unlocks.
//
// It is the only writer of total_xp. Every grant is keyed by an idempotency
// key so replaying a battle report cannot award experience twice.
package progress

import (
	"log/slog"
	"net/http"

	"gamebackend/pkg/api"
	"gamebackend/pkg/apierr"
	"gamebackend/pkg/httpx"
)

// Service is the progression domain service.
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

// GetProgressResponse is the progression read model.
type GetProgressResponse struct {
	Progress Progress `json:"progress"`
}

// GrantXPRequest is the body of POST /v1/progress/players/{player_id}/xp.
type GrantXPRequest struct {
	XP     int64  `json:"xp" doc:"Experience to add, greater than zero."`
	Reason string `json:"reason" doc:"Audit trail, e.g. match_reward."`
}

// GrantXPResponse reports the new progression.
type GrantXPResponse struct {
	Progress Progress `json:"progress"`
	Applied  bool     `json:"applied" doc:"false when the idempotency key was already used."`
}

// Register mounts the progression operations on r.
func (s *Service) Register(r *httpx.Router) {
	r.Handle(api.Op{
		ID:       "progress.get",
		Service:  "progress",
		Method:   http.MethodGet,
		Path:     "/v1/progress/players/{player_id}",
		Summary:  "Read a player's progression",
		Response: api.T(GetProgressResponse{}),
		Auth:     api.AuthPlayer,
	}, s.get)

	r.Handle(api.Op{
		ID:         "progress.grant_xp",
		Service:    "progress",
		Method:     http.MethodPost,
		Path:       "/v1/progress/players/{player_id}/xp",
		Summary:    "Grant experience (server-to-server, idempotent)",
		Request:    api.Body(GrantXPRequest{}),
		Response:   api.T(GrantXPResponse{}),
		Auth:       api.AuthService,
		Idempotent: true,
		Note:       "Requires an Idempotency-Key header; replaying the same key returns applied=false.",
	}, s.grantXP)
}

func (s *Service) get(r *http.Request) (any, error) {
	playerID := httpx.Param(r, "player_id")
	if err := httpx.RequireSelf(r, playerID); err != nil {
		return nil, err
	}
	player, err := s.store.Get(playerID)
	if err != nil {
		return nil, err
	}
	return GetProgressResponse{Progress: player}, nil
}

func (s *Service) grantXP(r *http.Request) (any, error) {
	if _, err := httpx.RequireService(r); err != nil {
		return nil, err
	}
	key, err := httpx.IdempotencyKey(r)
	if err != nil {
		return nil, err
	}
	playerID := httpx.Param(r, "player_id")
	var req GrantXPRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	if req.Reason == "" {
		return nil, apierr.Invalid("reason is required").WithDetail("field", "reason")
	}
	player, applied, err := s.store.GrantXP(playerID, key, req.XP, req.Reason)
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(r.Context(), "xp granted", "player_id", playerID, "xp", req.XP, "applied", applied)
	return GrantXPResponse{Progress: player, Applied: applied}, nil
}

// Grant applies an experience grant at the domain level.
//
// The HTTP handler and the in-process match fan-out call the same method, so
// there is exactly one place where progression rules live. When the service is
// split out, this becomes the HTTP call and the signature stays.
func (s *Service) Grant(playerID, idempotencyKey string, xp int64, reason string) (Progress, bool, error) {
	return s.store.GrantXP(playerID, idempotencyKey, xp, reason)
}
