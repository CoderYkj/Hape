// Package session owns the match lifecycle after matchmaking has picked the
// players: allocate a dedicated server, wait for it to report in, accept the
// authoritative battle report exactly once, then fan the result out.
//
// Authority boundary (.harness/rules/90-battle-integration.md): the UE5
// dedicated server is the only authority *inside* a match. This service is the
// only authority *about* a match, and it never trusts a client for either.
package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"gamebackend/pkg/api"
	"gamebackend/pkg/apierr"
	"gamebackend/pkg/battlealloc"
	"gamebackend/pkg/httpx"
	"gamebackend/pkg/idgen"
)

// ResultSink receives an accepted battle report. The platform wires it to
// progress, economy and rank; a message bus replaces it later without touching
// this package.
type ResultSink interface {
	MatchFinished(ctx context.Context, match FinishedMatch) error
}

// FinishedMatch is one accepted, authoritative battle report.
type FinishedMatch struct {
	SessionID       string
	MatchID         string
	ResultID        string
	Players         []PlayerResult
	DurationSeconds int64
	FinishedAt      time.Time
}

// Service is the session domain service.
type Service struct {
	log   *slog.Logger
	store Store
	alloc battlealloc.Allocator
	sink  ResultSink
	// meters is nil until the platform wires it: the counters below are only meaningful
	// on the /metrics endpoint, and a unit test that never reads them should not have to
	// build a registry.
	meters *httpx.Metrics

	now func() time.Time
}

// SetMetrics attaches the registry the session counters are published on, registering both
// at zero so "zero" is a queryable fact rather than an absent series.
func (s *Service) SetMetrics(m *httpx.Metrics) {
	s.meters = m
	if m == nil {
		return
	}
	m.AddCounter("allocations_failed", 0)
	m.AddCounter("session_sweep_races", 0)
}

// count increments one of the counters above when a registry is attached.
func (s *Service) count(name string) {
	if s.meters != nil {
		s.meters.AddCounter(name, 1)
	}
}

// New builds the service. sink may be nil (fan-out disabled in tests).
func New(log *slog.Logger, alloc battlealloc.Allocator, sink ResultSink) *Service {
	if alloc == nil {
		alloc = battlealloc.NewLocal()
	}
	return &Service{log: log, store: NewMemory(), alloc: alloc, sink: sink, now: time.Now}
}

// NewWithStore builds the service on a caller-supplied store.
func NewWithStore(log *slog.Logger, store Store, alloc battlealloc.Allocator, sink ResultSink) *Service {
	if alloc == nil {
		alloc = battlealloc.NewLocal()
	}
	return &Service{log: log, store: store, alloc: alloc, sink: sink, now: time.Now}
}

// CreateSessionRequest is the body of POST /v1/session/sessions.
type CreateSessionRequest struct {
	MatchID string   `json:"match_id" doc:"Match id from matchmaking.pop."`
	Players []string `json:"players" doc:"Player ids in the match, 2 to 10 unique ids."`
	Queue   string   `json:"queue" doc:"Queue the match came from, copied onto the session."`
	Region  string   `json:"region,omitempty" doc:"Preferred region; the allocator may override it."`
	Map     string   `json:"map,omitempty" doc:"Map identifier handed to the dedicated server."`
}

// CreateSessionResponse returns the session in the allocating state.
type CreateSessionResponse struct {
	Session Session `json:"session"`
	// RuntimeToken is the per-session credential the dedicated server presents when
	// it reports ready/results. Returned here (and only here) so a caller acting as
	// the DS - fakeds, or an operator debugging - can authenticate. The player-facing
	// read path never includes it.
	RuntimeToken string `json:"runtime_token" doc:"Per-session DS credential; also injected into the instance."`
}

// MarkReadyRequest is the body of POST /v1/session/sessions/{session_id}/ready.
// It is sent by the dedicated server (or the provider adapter) once the
// instance is listening.
type MarkReadyRequest struct {
	Address string `json:"address,omitempty" doc:"Address the dedicated server is listening on."`
	Port    int    `json:"port,omitempty" doc:"Port the dedicated server is listening on."`
}

// MarkReadyResponse returns the ready session.
type MarkReadyResponse struct {
	Session Session `json:"session"`
}

// SubmitResultRequest is the body of
// POST /v1/session/sessions/{session_id}/results.
type SubmitResultRequest struct {
	ResultID        string         `json:"result_id" doc:"Idempotency id chosen by the dedicated server, e.g. a UUID."`
	Players         []PlayerResult `json:"players" doc:"One entry per player in the session, no duplicates."`
	DurationSeconds int64          `json:"duration_seconds" doc:"Match duration in seconds."`
}

// SubmitResultResponse reports the finished session.
type SubmitResultResponse struct {
	Session Session `json:"session"`
	Applied bool    `json:"applied" doc:"false when the same result_id was already accepted."`
}

// GetSessionResponse returns one session.
type GetSessionResponse struct {
	Session Session `json:"session"`
}

// Register mounts the session operations on r.
func (s *Service) Register(r *httpx.Router) {
	r.Handle(api.Op{
		ID:       "session.create",
		Service:  "session",
		Method:   http.MethodPost,
		Path:     "/v1/session/sessions",
		Summary:  "Allocate a dedicated server for a match",
		Request:  api.Body(CreateSessionRequest{}),
		Response: api.T(CreateSessionResponse{}),
		Auth:     api.AuthService,
		Note:     "Not idempotent: each call allocates a dedicated server. Call it once per match_id; after a timeout, read the session back instead of creating a second one.",
	}, s.create)

	r.Handle(api.Op{
		ID:         "session.mark_ready",
		Service:    "session",
		Method:     http.MethodPost,
		Path:       "/v1/session/sessions/{session_id}/ready",
		Summary:    "Dedicated server reports it is listening",
		Request:    api.Body(MarkReadyRequest{}),
		Response:   api.T(MarkReadyResponse{}),
		Auth:       api.AuthService,
		Idempotent: true,
		Note:       "Requires the service token AND the session's X-Session-Token header: the first proves a trusted process, the second proves this is the DS of that match.",
	}, s.markReady)

	r.Handle(api.Op{
		ID:         "session.submit_result",
		Service:    "session",
		Method:     http.MethodPost,
		Path:       "/v1/session/sessions/{session_id}/results",
		Summary:    "Accept the authoritative battle report",
		Request:    api.Body(SubmitResultRequest{}),
		Response:   api.T(SubmitResultResponse{}),
		Auth:       api.AuthService,
		Idempotent: true,
		Note:       "Idempotent by result_id; requires the service token AND the session's X-Session-Token. Downstream fan-out runs inside one transaction, so a retry re-runs idempotent sinks.",
	}, s.submitResult)

	// A client learns its match id from the matchmaking ticket and nothing else, so
	// this is how it finds the server it has to connect to. Same scoping as
	// session.get: only players in the match can read it, and the report credential
	// never leaves the platform.
	r.Handle(api.Op{
		ID:       "session.get_by_match",
		Service:  "session",
		Method:   http.MethodGet,
		Path:     "/v1/session/sessions/by-match/{match_id}",
		Summary:  "Find your session by match id",
		Response: api.T(GetSessionResponse{}),
		Auth:     api.AuthPlayer,
		Note:     "The client-side read path: a ticket yields match_id, this yields the address to connect to.",
	}, s.getByMatch)

	r.Handle(api.Op{
		ID:       "session.get",
		Service:  "session",
		Method:   http.MethodGet,
		Path:     "/v1/session/sessions/{session_id}",
		Summary:  "Poll a session for its state and server address",
		Response: api.T(GetSessionResponse{}),
		Auth:     api.AuthPlayer,
	}, s.get)
}

func (s *Service) create(r *http.Request) (any, error) {
	if _, err := httpx.RequireService(r); err != nil {
		return nil, err
	}
	var req CreateSessionRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	if req.MatchID == "" {
		return nil, apierr.Invalid("match_id is required").WithDetail("field", "match_id")
	}
	if err := validatePlayers(req.Players); err != nil {
		return nil, err
	}

	// Time-based, not a process-local counter: with a persistent store, s_1 from
	// the previous process is still on disk, and a counter restarts at 1.
	// Not a bare timestamp: two concurrent creates in the same clock tick would get the same
	// id and the second one would 409. See pkg/idgen.
	id := idgen.New("s_")
	// Minted before allocation on purpose: a managed provider injects it into the
	// instance environment, so it has to exist by the time we ask for a server.
	runtimeToken, err := newRuntimeToken()
	if err != nil {
		return nil, apierr.Internal("could not mint a session token").WithCause(err)
	}

	_, err = s.store.Create(r.Context(), Session{
		ID:           id,
		MatchID:      req.MatchID,
		State:        StateAllocating,
		Players:      req.Players,
		Queue:        req.Queue,
		Region:       req.Region,
		Map:          req.Map,
		RuntimeToken: runtimeToken,
		CreatedAt:    s.now().UTC(),
	})
	if err != nil {
		return nil, err
	}

	allocation, err := s.alloc.Allocate(r.Context(), battlealloc.Request{
		SessionID:  id,
		MatchID:    req.MatchID,
		Queue:      req.Queue,
		Region:     req.Region,
		Map:        req.Map,
		MaxPlayers: len(req.Players),
		// The DS builds its report from the roster, so the roster travels with the
		// allocation (injected as PLAYERS). A UE5 server derives the same list from joins.
		Players:      req.Players,
		RuntimeToken: runtimeToken,
	})
	if err != nil {
		// Counted before the response: the SLO in docs/slo.md #4 is allocation success
		// rate, and this is the numerator's other half.
		s.count("allocations_failed")
		_, _ = s.store.Update(r.Context(), id, func(sp *Session) error {
			sp.State = StateFailed
			return nil
		})
		s.log.ErrorContext(r.Context(), "allocation failed", "session_id", id, "provider", s.alloc.Provider(), "err", err)
		return nil, apierr.Unavailable("dedicated server allocation failed").WithCause(err)
	}

	updated, err := s.store.Update(r.Context(), id, func(sp *Session) error {
		sp.AllocationID = allocation.ID
		sp.Provider = allocation.Provider
		sp.Address = allocation.Address
		sp.Port = allocation.Port
		// The ledger's left edge: without it "how long did this server run" and "is it still
		// unaccounted for" are both unanswerable.
		now := s.now().UTC()
		sp.AllocatedAt = &now
		return nil
	})
	if err != nil {
		return nil, err
	}

	// A slow allocation can outlive the stale sweep: a managed provider regularly takes
	// 20-90s, and if SESSION_STALE_AFTER is set below that, the sweep closes the session
	// while we are still waiting for a server. Returning a "failed" session that quietly
	// owns a running instance is how an allocation leaks until the vendor's TTL; instead,
	// record the allocation (so a failed release stays retryable) and hand the instance back.
	if updated.State != StateAllocating {
		s.count("session_sweep_races")
		s.log.WarnContext(r.Context(), "allocation finished after the session closed; releasing the server",
			"session_id", id, "allocation_id", allocation.ID, "state", updated.State)
		if releaseErr := s.alloc.Release(r.Context(), allocation.ID); releaseErr != nil {
			s.log.ErrorContext(r.Context(), "releasing the orphaned allocation failed; scheduling a retry",
				"session_id", id, "allocation_id", allocation.ID, "err", releaseErr)
			s.markReleasePending(r.Context(), id, releaseErr)
		} else {
			s.markReleased(r.Context(), id)
		}
		return nil, apierr.Unavailable("the session closed while its server was starting; the allocation was released").
			WithDetail("session_id", id).
			WithDetail("session_state", updated.State)
	}

	s.log.InfoContext(r.Context(), "session allocating", "session_id", id, "match_id", req.MatchID,
		"provider", allocation.Provider, "address", allocation.Address)
	return CreateSessionResponse{Session: updated, RuntimeToken: updated.RuntimeToken}, nil
}

func (s *Service) markReady(r *http.Request) (any, error) {
	if _, err := httpx.RequireService(r); err != nil {
		return nil, err
	}
	sessionID := httpx.Param(r, "session_id")
	var req MarkReadyRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}

	current, err := s.store.Get(r.Context(), sessionID)
	if err != nil {
		return nil, err
	}
	if err := requireRuntimeToken(r, current.RuntimeToken); err != nil {
		return nil, err
	}

	updated, err := s.store.Update(r.Context(), sessionID, func(sp *Session) error {
		if !CanTransition(sp.State, StateReady) {
			return ErrInvalidTransition.WithDetail("from", sp.State).WithDetail("to", StateReady)
		}
		if sp.State == StateReady {
			return nil
		}
		if req.Address != "" {
			sp.Address = req.Address
		}
		if req.Port != 0 {
			sp.Port = req.Port
		}
		sp.State = StateReady
		now := s.now().UTC()
		sp.ReadyAt = &now
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.log.InfoContext(r.Context(), "session ready", "session_id", sessionID, "address", updated.Address, "port", updated.Port)
	return MarkReadyResponse{Session: updated}, nil
}

func (s *Service) submitResult(r *http.Request) (any, error) {
	if _, err := httpx.RequireService(r); err != nil {
		return nil, err
	}
	sessionID := httpx.Param(r, "session_id")
	var req SubmitResultRequest
	if err := httpx.Decode(r, &req); err != nil {
		return nil, err
	}
	if req.ResultID == "" {
		return nil, apierr.Invalid("result_id is required").WithDetail("field", "result_id")
	}
	if req.DurationSeconds < 0 {
		return nil, apierr.Invalid("duration_seconds must not be negative").WithDetail("field", "duration_seconds")
	}

	current, err := s.store.Get(r.Context(), sessionID)
	if err != nil {
		return nil, err
	}
	// Dual credentials (ADR 0007 adjacent decision): the service token proves "a
	// trusted process", the session token proves "the DS of *this* match". A
	// compromised instance can therefore only speak for its own session.
	if err := requireRuntimeToken(r, current.RuntimeToken); err != nil {
		return nil, err
	}
	if current.ResultID == req.ResultID {
		// Settlement logs must say what was (not) applied, so an on-call reader
		// can tell a replay from a first landing without reading the payload.
		s.log.InfoContext(r.Context(), "battle report replayed", "session_id", sessionID,
			"result_id", req.ResultID, "applied", false)
		return SubmitResultResponse{Session: current, Applied: false}, nil
	}
	if current.ResultID != "" {
		return nil, apierr.Conflict("a different result is already recorded").
			WithDetail("recorded_result_id", current.ResultID)
	}
	if !CanTransition(current.State, StateFinished) {
		return nil, ErrInvalidTransition.WithDetail("from", current.State).WithDetail("to", StateFinished)
	}
	if err := validateResults(current.Players, req.Players); err != nil {
		return nil, err
	}

	sessionState := current.State
	if sessionState == StateReady {
		// A short match can finish without an explicit join event.
		sessionState = StateInProgress
	}
	if sessionState != StateInProgress {
		return nil, ErrInvalidTransition.WithDetail("from", current.State).WithDetail("to", StateFinished)
	}

	// Record the report before the fan-out: if the rewards fail and the caller
	// retries, the retry must find the same report on file. Recording is
	// idempotent by result_id, so a duplicate is a no-op.
	if recorder, ok := s.store.(ResultRecorder); ok {
		if err := recorder.RecordResult(r.Context(), FinishedMatch{
			SessionID:       sessionID,
			MatchID:         current.MatchID,
			ResultID:        req.ResultID,
			Players:         req.Players,
			DurationSeconds: req.DurationSeconds,
			FinishedAt:      s.now().UTC(),
		}); err != nil {
			s.log.ErrorContext(r.Context(), "recording the battle report failed",
				"session_id", sessionID, "result_id", req.ResultID, "err", err)
			return nil, apierr.Unavailable("recording the battle report failed, retry with the same result_id").WithCause(err)
		}
	}

	if s.sink != nil {
		err := s.sink.MatchFinished(r.Context(), FinishedMatch{
			SessionID:       sessionID,
			MatchID:         current.MatchID,
			ResultID:        req.ResultID,
			Players:         req.Players,
			DurationSeconds: req.DurationSeconds,
			FinishedAt:      s.now().UTC(),
		})
		if err != nil {
			// The session stays open on purpose: the report was received, the
			// downstream fan-out was not. The caller retries, and every sink
			// deduplicates by result_id.
			s.log.ErrorContext(r.Context(), "result fan-out failed", "session_id", sessionID, "result_id", req.ResultID, "err", err)
			return nil, apierr.Unavailable("result fan-out failed, retry with the same result_id").WithCause(err)
		}
	}

	updated, err := s.store.Update(r.Context(), sessionID, func(sp *Session) error {
		sp.State = StateFinished
		sp.ResultID = req.ResultID
		now := s.now().UTC()
		sp.FinishedAt = &now
		return nil
	})
	if err != nil {
		return nil, err
	}

	// The match is over, so the dedicated server is no longer needed. Never fail the
	// request over a release: the report is already accepted. But a failed release is
	// also not just a log line - a server nobody stopped keeps billing - so it is
	// recorded and the reconciler retries it.
	if updated.AllocationID != "" {
		if err := s.alloc.Release(r.Context(), updated.AllocationID); err != nil {
			s.log.WarnContext(r.Context(), "release after match failed; scheduling a retry",
				"session_id", sessionID, "allocation_id", updated.AllocationID, "err", err)
			s.markReleasePending(r.Context(), sessionID, err)
		} else {
			// The money path deserves a line of its own: "we confirmed this instance is gone"
			// is what an operator greps for after a billing question, and until now only the
			// failures were logged.
			s.log.InfoContext(r.Context(), "match server released",
				"session_id", sessionID, "allocation_id", updated.AllocationID)
			s.markReleased(r.Context(), sessionID)
		}
	}

	s.log.InfoContext(r.Context(), "session finished", "session_id", sessionID,
		"result_id", req.ResultID, "applied", true)
	return SubmitResultResponse{Session: updated, Applied: true}, nil
}

// ResultRecorder is an optional store capability: keep the authoritative report
// itself, not merely the rewards it triggered.
//
// The rewards are auditable on their own (ledger + idempotency keys), but "why did
// this player get 100 gold" is answered by the report. Without it, the ledger says
// a match happened and nothing says what the match was.
type ResultRecorder interface {
	RecordResult(ctx context.Context, match FinishedMatch) error
}

// StaleSession is one session the reconciler declared dead.
type StaleSession struct {
	ID           string
	Provider     string
	AllocationID string
}

// ErrNoExpirer is returned when the active store cannot enumerate sessions, so
// a sweep is impossible in this deployment.
var ErrNoExpirer = errors.New("session store cannot enumerate stale sessions")

// ExpireStale fails sessions that sat in allocating or ready for longer than
// staleAfter, and releases their dedicated servers.
//
// Safe to run from a timer: the state change goes through CanTransition, and the
// release is best-effort (a provider that already dropped the instance returns
// an error we log instead of failing the whole sweep).
func (s *Service) ExpireStale(ctx context.Context, now time.Time, staleAfter time.Duration) ([]StaleSession, error) {
	if staleAfter <= 0 {
		return nil, fmt.Errorf("session: staleAfter must be positive, got %s", staleAfter)
	}
	expirer, ok := s.store.(Expirer)
	if !ok {
		return nil, ErrNoExpirer
	}

	expired, err := expirer.ExpireStale(ctx, now.Add(-staleAfter))
	if err != nil {
		return nil, err
	}
	out := make([]StaleSession, 0, len(expired))
	for _, stale := range expired {
		if err := s.alloc.Release(ctx, stale.AllocationID); err != nil {
			s.log.WarnContext(ctx, "release after expiry failed; scheduling a retry",
				"session_id", stale.ID, "allocation_id", stale.AllocationID, "err", err)
			s.markReleasePending(ctx, stale.ID, err)
		} else {
			s.log.InfoContext(ctx, "expired session's server released",
				"session_id", stale.ID, "allocation_id", stale.AllocationID)
			s.markReleased(ctx, stale.ID)
		}
		s.log.WarnContext(ctx, "session expired", "session_id", stale.ID, "match_id", stale.MatchID,
			"provider", stale.Provider, "allocation_id", stale.AllocationID, "stale_state", StateFailed)
		out = append(out, StaleSession{ID: stale.ID, Provider: stale.Provider, AllocationID: stale.AllocationID})
	}
	return out, nil
}

func (s *Service) get(r *http.Request) (any, error) {
	playerID, err := httpx.RequirePlayer(r)
	if err != nil {
		return nil, err
	}
	found, err := s.store.Get(r.Context(), httpx.Param(r, "session_id"))
	if err != nil {
		return nil, err
	}
	return playerView(playerID, found)
}

func (s *Service) getByMatch(r *http.Request) (any, error) {
	playerID, err := httpx.RequirePlayer(r)
	if err != nil {
		return nil, err
	}
	found, err := s.store.GetByMatch(r.Context(), httpx.Param(r, "match_id"))
	if err != nil {
		return nil, err
	}
	return playerView(playerID, found)
}

// playerView is the only way a session reaches a player: the report credential is
// stripped, and a player outside the roster cannot read the session at all.
func playerView(playerID string, found Session) (any, error) {
	// Never hand the report credential to a player.
	found.RuntimeToken = ""
	for _, id := range found.Players {
		if id == playerID {
			return GetSessionResponse{Session: found}, nil
		}
	}
	return nil, apierr.PermissionDenied("player is not in this session")
}

// newRuntimeToken mints the per-session credential: 32 random bytes, URL-safe so it
// survives being pasted into an env var and a header.
func newRuntimeToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// requireRuntimeToken enforces the per-session credential. A session without one is
// refused rather than accepted: fail closed, because the only way to have no token
// is a session created by an older build or a corrupted row.
func requireRuntimeToken(r *http.Request, want string) error {
	if want == "" {
		return apierr.Unauthenticated("session has no runtime token; recreate the session")
	}
	presented := r.Header.Get("X-Session-Token")
	if subtle.ConstantTimeCompare([]byte(presented), []byte(want)) != 1 {
		return apierr.Unauthenticated("missing or invalid X-Session-Token")
	}
	return nil
}

func validatePlayers(players []string) error {
	if len(players) < 2 {
		return apierr.Invalid("players must contain at least 2 ids").WithDetail("field", "players")
	}
	if len(players) > 10 {
		return apierr.Invalid("players must contain at most 10 ids").WithDetail("field", "players")
	}
	seen := map[string]bool{}
	for _, id := range players {
		if id == "" {
			return apierr.Invalid("player ids must not be empty").WithDetail("field", "players")
		}
		if seen[id] {
			return apierr.Invalid("duplicate player id in players").WithDetail("player_id", id)
		}
		seen[id] = true
	}
	return nil
}

func validateResults(expected []string, results []PlayerResult) error {
	if len(results) != len(expected) {
		return apierr.Invalid("results must cover every player exactly once").
			WithDetail("expected", fmt.Sprintf("%d", len(expected))).
			WithDetail("got", fmt.Sprintf("%d", len(results)))
	}
	seen := map[string]bool{}
	for _, result := range results {
		if seen[result.PlayerID] {
			return apierr.Invalid("duplicate player in results").WithDetail("player_id", result.PlayerID)
		}
		seen[result.PlayerID] = true
	}
	for _, id := range expected {
		if !seen[id] {
			return apierr.Invalid("results must cover every player exactly once").WithDetail("missing_player_id", id)
		}
	}
	return nil
}

// ErrNoReleaseTracker is returned when the active store cannot enumerate sessions with
// a pending release, so the retry sweep is impossible in this deployment.
var ErrNoReleaseTracker = errors.New("session store cannot enumerate pending releases")

// markReleasePending records that a dedicated server was not confirmed gone.
//
// Recording is best effort and logged rather than returned: every caller is already
// past the point where failing the request could help, and losing the retry record is
// strictly better than losing the response.
func (s *Service) markReleasePending(ctx context.Context, sessionID string, cause error) {
	_, err := s.store.Update(ctx, sessionID, func(sp *Session) error {
		sp.ReleasePending = true
		sp.ReleaseAttempts++
		return nil
	})
	if err != nil {
		s.log.ErrorContext(ctx, "could not record a pending release",
			"session_id", sessionID, "cause", cause.Error(), "err", err)
	}
}

// ReleaseRetryResult is one retry sweep's outcome. It is a struct rather than a bare
// count because the failure half is a metric, not a detail.
type ReleaseRetryResult struct {
	// Cleared is how many releases were confirmed on this pass.
	Cleared int
	// Failed is how many are still unconfirmed. A non-zero value that persists is the
	// signal the alert rule watches.
	Failed int
}

// ReleaseStats is the current release backlog: the population, the worst offender, and
// how long the oldest one has been waiting.
//
// It is computed from sessions that are already flagged, so it costs one partial-index
// scan, and it is allowed to be empty - a healthy deployment has nothing pending.
type ReleaseStats struct {
	Pending       int
	MaxAttempts   int
	OldestSeconds int64
}

// RetryReleases re-attempts the releases no provider ever confirmed. Safe to run from a
// timer: releasing an instance that is already gone is a success, so a retry can never
// make things worse.
//
// This is the only place a billing leak is visible before the invoice: ReleaseAttempts
// climbing on the same session means the provider is not letting go.
func (s *Service) RetryReleases(ctx context.Context, limit int) (ReleaseRetryResult, error) {
	tracker, ok := s.store.(ReleaseTracker)
	if !ok {
		return ReleaseRetryResult{}, ErrNoReleaseTracker
	}
	pending, err := tracker.PendingReleases(ctx, limit)
	if err != nil {
		return ReleaseRetryResult{}, err
	}

	result := ReleaseRetryResult{}
	for _, sp := range pending {
		if err := s.alloc.Release(ctx, sp.AllocationID); err != nil {
			s.log.WarnContext(ctx, "release retry failed",
				"session_id", sp.ID, "allocation_id", sp.AllocationID,
				"attempts", sp.ReleaseAttempts+1, "err", err)
			s.markReleasePending(ctx, sp.ID, err)
			result.Failed++
			continue
		}
		if err := s.markReleasedErr(ctx, sp.ID); err != nil {
			s.log.ErrorContext(ctx, "release succeeded but the pending flag would not clear",
				"session_id", sp.ID, "err", err)
			continue
		}
		s.log.InfoContext(ctx, "pending release confirmed",
			"session_id", sp.ID, "allocation_id", sp.AllocationID, "attempts", sp.ReleaseAttempts+1)
		result.Cleared++
	}
	return result, nil
}

// ReleaseStats reports the current release backlog for gauges and alerts.
//
// "Oldest" is measured from when the session finished, or from its creation when it
// never got that far: that is when the release should have happened, so the number is an
// upper bound on how long a server has been unaccounted for.
func (s *Service) ReleaseStats(ctx context.Context, now time.Time) (ReleaseStats, error) {
	tracker, ok := s.store.(ReleaseTracker)
	if !ok {
		return ReleaseStats{}, ErrNoReleaseTracker
	}
	pending, err := tracker.PendingReleases(ctx, 0)
	if err != nil {
		return ReleaseStats{}, err
	}

	stats := ReleaseStats{Pending: len(pending)}
	for _, sp := range pending {
		if sp.ReleaseAttempts > stats.MaxAttempts {
			stats.MaxAttempts = sp.ReleaseAttempts
		}
		since := sp.CreatedAt
		if sp.FinishedAt != nil {
			since = *sp.FinishedAt
		}
		if seconds := int64(now.Sub(since).Seconds()); seconds > stats.OldestSeconds {
			stats.OldestSeconds = seconds
		}
	}
	return stats, nil
}

// ErrNoCensus is returned when the store cannot report session state populations.
var ErrNoCensus = errors.New("session store cannot report a state census")

// ErrNoAllocationLedger is returned when the active store cannot enumerate allocations, so
// the ledger report is impossible in this deployment.
var ErrNoAllocationLedger = errors.New("session store cannot enumerate allocations")

// markReleased records that the provider confirmed the instance is gone and clears the retry
// flag in the same write.
func (s *Service) markReleased(ctx context.Context, sessionID string) {
	if err := s.markReleasedErr(ctx, sessionID); err != nil {
		// The server is already gone; failing to write the timestamp is a bookkeeping miss,
		// not a reason to fail whatever asked for the release.
		s.log.ErrorContext(ctx, "could not record the confirmed release", "session_id", sessionID, "err", err)
	}
}

func (s *Service) markReleasedErr(ctx context.Context, sessionID string) error {
	now := s.now().UTC()
	_, err := s.store.Update(ctx, sessionID, func(sp *Session) error {
		sp.ReleasedAt = &now
		sp.ReleasePending = false
		return nil
	})
	return err
}

// AllocationRow is one line of the allocation ledger, with the numbers an operator or a
// finance conversation needs already derived.
type AllocationRow struct {
	SessionID       string     `json:"session_id"`
	Provider        string     `json:"provider"`
	AllocationID    string     `json:"allocation_id"`
	State           string     `json:"state"`
	AllocatedAt     time.Time  `json:"allocated_at"`
	ReleasedAt      *time.Time `json:"released_at,omitempty"`
	ReleasePending  bool       `json:"release_pending"`
	ReleaseAttempts int        `json:"release_attempts"`
	// InstanceSeconds is allocated -> released, or allocated -> now while unreleased.
	InstanceSeconds int64 `json:"instance_seconds"`
	// Unreleased means the provider never confirmed the stop. It is the only column that
	// can still be billing.
	Unreleased bool `json:"unreleased"`
	// UnreleasedFor is how long it has been unconfirmed (zero when released).
	UnreleasedFor time.Duration `json:"-"`
}

// Allocations returns the allocation ledger: what we asked for, when it came up, and whether
// anyone confirmed it stopped.
//
// This is the half of billing reconciliation that does not need the vendor's invoice: it
// answers "which instances are unaccounted for, and how long did each run".
func (s *Service) Allocations(ctx context.Context, q AllocationQuery, now time.Time) ([]AllocationRow, error) {
	ledger, ok := s.store.(AllocationLedger)
	if !ok {
		return nil, ErrNoAllocationLedger
	}
	entries, err := ledger.Allocations(ctx, q)
	if err != nil {
		return nil, err
	}

	rows := make([]AllocationRow, 0, len(entries))
	for _, session := range entries {
		if session.AllocatedAt == nil {
			continue
		}
		row := AllocationRow{
			SessionID:       session.ID,
			Provider:        session.Provider,
			AllocationID:    session.AllocationID,
			State:           session.State,
			AllocatedAt:     *session.AllocatedAt,
			ReleasedAt:      session.ReleasedAt,
			ReleasePending:  session.ReleasePending,
			ReleaseAttempts: session.ReleaseAttempts,
		}
		end := now
		if session.ReleasedAt != nil {
			end = *session.ReleasedAt
		} else {
			row.Unreleased = true
			row.UnreleasedFor = now.Sub(*session.AllocatedAt)
		}
		if seconds := int64(end.Sub(*session.AllocatedAt).Seconds()); seconds > 0 {
			row.InstanceSeconds = seconds
		}
		rows = append(rows, row)
	}
	return rows, nil
}

// Census reports the population of the states that can leak a server or strand a player.
//
// The gauge names it feeds are the input to two alerts: "allocating for minutes" (a provider
// that is not answering) and "ready/in_progress for half an hour" (a match nobody finished).
func (s *Service) Census(ctx context.Context, now time.Time) (StateCensus, error) {
	provider, ok := s.store.(CensusProvider)
	if !ok {
		return StateCensus{}, ErrNoCensus
	}
	return provider.Census(ctx, now)
}
