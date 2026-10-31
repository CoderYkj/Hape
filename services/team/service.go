// Package team owns persistent squads: who is in a team, who owns it and the
// invariants that matchmaking relies on (size, unique membership).
package team

import (
	"log/slog"
	"net/http"

	"gamebackend/pkg/api"
	"gamebackend/pkg/httpx"
)

// Service is the team domain service.
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

// CreateTeamRequest is the body of POST /v1/team/teams.
type CreateTeamRequest struct {
	Name string `json:"name" doc:"Team name, 1-32 characters."`
}

// CreateTeamResponse returns the created team.
type CreateTeamResponse struct {
	Team Team `json:"team"`
}

// JoinTeamResponse returns the team after the join attempt.
type JoinTeamResponse struct {
	Team    Team `json:"team"`
	Applied bool `json:"applied" doc:"false when the caller was already a member; the call is idempotent."`
}

// GetTeamResponse returns one team.
type GetTeamResponse struct {
	Team Team `json:"team"`
}

// Register mounts the team operations on r.
func (s *Service) Register(r *httpx.Router) {
	r.Handle(api.Op{
		ID:       "team.create",
		Service:  "team",
		Method:   http.MethodPost,
		Path:     "/v1/team/teams",
		Summary:  "Create a team owned by the caller",
		Request:  api.Body(CreateTeamRequest{}),
		Response: api.T(CreateTeamResponse{}),
		Auth:     api.AuthPlayer,
		Note:     "Not idempotent: every call creates a new team. A client that retries after a timeout can create a duplicate, so it must read back team membership before retrying.",
	}, s.create)

	r.Handle(api.Op{
		ID:         "team.join",
		Service:    "team",
		Method:     http.MethodPost,
		Path:       "/v1/team/teams/{team_id}/members",
		Summary:    "Join a team as the caller (idempotent)",
		Response:   api.T(JoinTeamResponse{}),
		Auth:       api.AuthPlayer,
		Idempotent: true,
	}, s.join)

	r.Handle(api.Op{
		ID:       "team.get",
		Service:  "team",
		Method:   http.MethodGet,
		Path:     "/v1/team/teams/{team_id}",
		Summary:  "Read a team and its members",
		Response: api.T(GetTeamResponse{}),
		Auth:     api.AuthPlayer,
	}, s.get)
}

func (s *Service) create(r *http.Request) (any, error) {
	playerID, err := httpx.RequirePlayer(r)
	if err != nil {
		return nil, err
	}
	var req CreateTeamRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	name, err := ValidateName(req.Name)
	if err != nil {
		return nil, err
	}
	created, err := s.store.Create(playerID, name)
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(r.Context(), "team created", "team_id", created.ID, "owner_id", playerID)
	return CreateTeamResponse{Team: created}, nil
}

func (s *Service) join(r *http.Request) (any, error) {
	playerID, err := httpx.RequirePlayer(r)
	if err != nil {
		return nil, err
	}
	teamID := httpx.Param(r, "team_id")
	joined, applied, err := s.store.Join(teamID, playerID)
	if err != nil {
		return nil, err
	}
	return JoinTeamResponse{Team: joined, Applied: applied}, nil
}

func (s *Service) get(r *http.Request) (any, error) {
	if _, err := httpx.RequirePlayer(r); err != nil {
		return nil, err
	}
	found, err := s.store.Get(httpx.Param(r, "team_id"))
	if err != nil {
		return nil, err
	}
	return GetTeamResponse{Team: found}, nil
}
