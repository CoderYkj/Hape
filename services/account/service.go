// Package account owns player identity and the editable profile.
//
// Boundary: Nakama authenticates (device, Steam, Apple, ...). This service
// turns a proven external identity into a backend Player and is the only
// service allowed to write profile fields.
package account

import (
	"log/slog"
	"net/http"
	"strings"

	"gamebackend/pkg/api"
	"gamebackend/pkg/apierr"
	"gamebackend/pkg/httpx"
)

// Service is the account domain service.
type Service struct {
	log   *slog.Logger
	store Store
}

// New builds the service with the in-memory store.
func New(log *slog.Logger) *Service {
	return &Service{log: log, store: NewMemory()}
}

// NewWithStore builds the service on a caller-supplied store (tests, SQL later).
func NewWithStore(log *slog.Logger, store Store) *Service {
	return &Service{log: log, store: store}
}

// LoginRequest is the body of POST /v1/account/sessions.
type LoginRequest struct {
	ExternalID  string `json:"external_id" doc:"Nakama user id proven by the Nakama session."`
	DisplayName string `json:"display_name,omitempty" doc:"Optional first-time display name; ignored for existing players."`
}

// LoginResponse is returned once per device login.
type LoginResponse struct {
	Player       Player `json:"player"`
	SessionToken string `json:"session_token" doc:"Player token for subsequent calls. Dev builds return dev-player-<id>; production returns a signed JWT."`
}

// GetPlayerResponse is the profile read model.
type GetPlayerResponse struct {
	Player Player `json:"player"`
}

// UpdateProfileRequest is the body of PATCH /v1/account/players/{player_id}.
type UpdateProfileRequest struct {
	DisplayName string `json:"display_name" doc:"New display name, 1-24 characters after trimming."`
}

// UpdateProfileResponse is the updated profile.
type UpdateProfileResponse struct {
	Player Player `json:"player"`
}

// Register mounts the account operations on r.
func (s *Service) Register(r *httpx.Router) {
	r.Handle(api.Op{
		ID:         "account.login",
		Service:    "account",
		Method:     http.MethodPost,
		Path:       "/v1/account/sessions",
		Summary:    "Exchange a verified Nakama identity for an internal player session",
		Request:    api.Body(LoginRequest{}),
		Response:   api.T(LoginResponse{}),
		Auth:       api.AuthPublic,
		Idempotent: true,
		Note:       "Idempotent by external_id: the same Nakama user always maps to the same player.",
	}, s.login)

	r.Handle(api.Op{
		ID:       "account.get_player",
		Service:  "account",
		Method:   http.MethodGet,
		Path:     "/v1/account/players/{player_id}",
		Summary:  "Read a player profile",
		Response: api.T(GetPlayerResponse{}),
		Auth:     api.AuthPlayer,
	}, s.getPlayer)

	r.Handle(api.Op{
		ID:         "account.update_profile",
		Service:    "account",
		Method:     http.MethodPatch,
		Path:       "/v1/account/players/{player_id}",
		Summary:    "Change the caller's display name",
		Request:    api.Body(UpdateProfileRequest{}),
		Response:   api.T(UpdateProfileResponse{}),
		Auth:       api.AuthPlayer,
		Idempotent: true,
	}, s.updateProfile)
}

func (s *Service) login(r *http.Request) (any, error) {
	var req LoginRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	externalID := strings.TrimSpace(req.ExternalID)
	if externalID == "" {
		return nil, apierr.Invalid("external_id is required").WithDetail("field", "external_id")
	}

	player, created, err := s.store.FindOrCreate(r.Context(), externalID, strings.TrimSpace(req.DisplayName))
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(r.Context(), "player session issued", "player_id", player.ID, "created", created)
	return LoginResponse{Player: player, SessionToken: "dev-player-" + player.ID}, nil
}

func (s *Service) getPlayer(r *http.Request) (any, error) {
	playerID := httpx.Param(r, "player_id")
	if err := httpx.RequireSelf(r, playerID); err != nil {
		return nil, err
	}
	player, err := s.store.GetByID(r.Context(), playerID)
	if err != nil {
		return nil, err
	}
	return GetPlayerResponse{Player: player}, nil
}

func (s *Service) updateProfile(r *http.Request) (any, error) {
	playerID := httpx.Param(r, "player_id")
	if err := httpx.RequireSelf(r, playerID); err != nil {
		return nil, err
	}
	var req UpdateProfileRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	name, err := ValidateDisplayName(req.DisplayName)
	if err != nil {
		return nil, err
	}
	player, err := s.store.UpdateDisplayName(r.Context(), playerID, name)
	if err != nil {
		return nil, err
	}
	return UpdateProfileResponse{Player: player}, nil
}
