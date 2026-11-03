package battlealloc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Edgegap is the first real managed-provider adapter.
//
// The whole point of pkg/battlealloc is that this file is the only place that knows
// Edgegap exists: the session service asks for "a server", and gets an address.
//
// API shape (Edgegap v1, verified against the live API while wiring this):
//
//	POST   /v1/deploy            -> request_id
//	GET    /v1/status/{id}       -> running, public_ip, ports{...}, error
//	DELETE /v1/stop/{id}         -> 200/404 (404 = already gone)
//
// Authentication is `Authorization: Token <token>` - not Bearer. That one detail
// cost a 401 round trip to discover, so it is written down here.
type Edgegap struct {
	token     string
	latitude  *float64
	longitude *float64
	app       string
	version   string
	baseURL   string
	client    *http.Client
	poll      time.Duration
	readyWait time.Duration
	stopWait  time.Duration
	stopPoll  time.Duration
	portName  string
	portProto string
	portNum   int
	now       func() time.Time
}

// DefaultReadyTimeout is how long an allocation may take before it is abandoned. It is
// exported because it is also the lower bound for the session stale threshold: sweeping a
// session while its allocation is still in flight is a race, and the session service now
// detects it, but a configuration where it *cannot* happen is better.
const DefaultReadyTimeout = 3 * time.Minute

// EdgegapConfig configures the adapter. Zero values get sensible defaults, except
// Token/AppName which are required.
type EdgegapConfig struct {
	Token   string
	AppName string
	// Version is optional; Edgegap deploys the latest version when empty.
	Version string
	// BaseURL defaults to https://api.edgegap.com.
	BaseURL string
	// Client defaults to a 15s-timeout HTTP client.
	Client *http.Client
	// PollInterval defaults to 2s, ReadyTimeout to 3 minutes. Managed providers
	// regularly take 20-90s to become ready, so a 10s timeout would be wrong.
	PollInterval time.Duration
	ReadyTimeout time.Duration
	// StopTimeout bounds the read-back after a stop request. A 200 on DELETE only means
	// the provider accepted the request; the instance may still be running and billing,
	// so the release is not confirmed until the status endpoint agrees.
	StopTimeout time.Duration
	// StopPollInterval is how often the deployment status is re-read while waiting.
	StopPollInterval time.Duration
	// PortName/PortProto/PortNum identify the game port in the deployment. The
	// Edgegap app registers it as name=gameport, 7777/UDP.
	PortName  string
	PortProto string
	PortNum   int
	// Latitude/Longitude are the fallback location centroid. Edgegap refuses a
	// deployment with no placement hint at all, so when the caller has no player IPs
	// we send this instead of failing. Set both or neither.
	Latitude  *float64
	Longitude *float64
	Now       func() time.Time
}

// NewEdgegap validates the configuration and returns the adapter.
func NewEdgegap(cfg EdgegapConfig) (*Edgegap, error) {
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, errors.New("battlealloc: edgegap token is required")
	}
	if strings.TrimSpace(cfg.AppName) == "" {
		return nil, errors.New("battlealloc: edgegap app name is required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.edgegap.com"
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = DefaultReadyTimeout
	}
	if cfg.StopTimeout <= 0 {
		// Teardown is usually much faster than startup, but it is not instant and the
		// cost of guessing too short is a false "still running" alarm.
		cfg.StopTimeout = 30 * time.Second
	}
	if cfg.StopPollInterval <= 0 {
		cfg.StopPollInterval = time.Second
	}
	if cfg.PortName == "" {
		cfg.PortName = "gameport"
	}
	if cfg.PortProto == "" {
		cfg.PortProto = "UDP"
	}
	if cfg.PortNum == 0 {
		cfg.PortNum = 7777
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	// Both coordinates or none: a half-configured centroid is a typo, not a choice.
	if (cfg.Latitude == nil) != (cfg.Longitude == nil) {
		return nil, errors.New("battlealloc: edgegap latitude and longitude must be set together")
	}
	return &Edgegap{
		token:     cfg.Token,
		latitude:  cfg.Latitude,
		longitude: cfg.Longitude,
		app:       cfg.AppName,
		version:   cfg.Version,
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		client:    cfg.Client,
		poll:      cfg.PollInterval,
		readyWait: cfg.ReadyTimeout,
		stopWait:  cfg.StopTimeout,
		stopPoll:  cfg.StopPollInterval,
		portName:  cfg.PortName,
		portProto: strings.ToUpper(cfg.PortProto),
		portNum:   cfg.PortNum,
		now:       cfg.Now,
	}, nil
}

// Provider implements Allocator.
func (e *Edgegap) Provider() string { return "edgegap" }

// Allocate implements Allocator: deploy, then poll until the instance is reachable.
//
// On timeout the instance is stopped before returning: leaving a half-started
// deployment behind is exactly the leak this adapter exists to avoid.
func (e *Edgegap) Allocate(ctx context.Context, req Request) (Allocation, error) {
	if req.SessionID == "" {
		return Allocation{}, fmt.Errorf("%w: session id is required", ErrPermanent)
	}

	requestID, err := e.deploy(ctx, req)
	if err != nil {
		return Allocation{}, err
	}

	deadline := e.now().Add(e.readyWait)
	for {
		status, gone, err := e.status(ctx, requestID)
		if gone {
			return Allocation{}, fmt.Errorf("%w: edgegap deployment %s disappeared while starting", ErrPermanent, requestID)
		}
		if err != nil {
			if errors.Is(err, ErrRetryable) {
				// A transient status read must not abandon a live deployment.
				if e.now().Before(deadline) {
					if waitErr := sleep(ctx, e.poll); waitErr != nil {
						return Allocation{}, waitErr
					}
					continue
				}
			}
			return Allocation{}, err
		}

		if status.Error {
			// The provider gave up: stop it (harmless if already gone) and report.
			_ = e.stop(ctx, requestID)
			return Allocation{}, fmt.Errorf("%w: edgegap deployment failed: %s",
				ErrPermanent, firstNonEmpty(status.ErrorDetail, status.CurrentStatus))
		}

		if status.Running {
			allocation, err := e.allocationFrom(requestID, status)
			if err != nil {
				_ = e.stop(ctx, requestID)
				return Allocation{}, err
			}
			return allocation, nil
		}

		if !e.now().Before(deadline) {
			_ = e.stop(ctx, requestID)
			return Allocation{}, fmt.Errorf("%w: edgegap deployment %s not ready after %s (status %q)",
				ErrRetryable, requestID, e.readyWait, status.CurrentStatus)
		}
		if err := sleep(ctx, e.poll); err != nil {
			_ = e.stop(ctx, requestID)
			return Allocation{}, err
		}
	}
}

// Release implements Allocator.
//
// Stopping is two steps: ask, then read back until the provider agrees the instance is
// gone. The stop request alone is not a release - a deployment that is still running is
// still being billed, and "we sent a DELETE" is not something an operator can audit.
func (e *Edgegap) Release(ctx context.Context, allocationID string) error {
	if err := e.stop(ctx, allocationID); err != nil {
		return err
	}
	return e.waitStopped(ctx, allocationID)
}

// stop issues the stop request without waiting for the read-back.
//
// Used where nothing could retry a confirmation (an allocation that failed before a
// session ever recorded it), and by Release before it waits.
func (e *Edgegap) stop(ctx context.Context, allocationID string) error {
	if allocationID == "" {
		return fmt.Errorf("%w: allocation id is required", ErrPermanent)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		e.baseURL+"/v1/stop/"+allocationID, nil)
	if err != nil {
		return fmt.Errorf("%w: build stop request: %v", ErrPermanent, err)
	}
	e.authorize(req)

	resp, err := e.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: edgegap stop: %v", ErrRetryable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)

	switch {
	case resp.StatusCode == http.StatusNotFound:
		// Already gone. That is the desired end state.
		return nil
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return fmt.Errorf("%w: edgegap stop: HTTP %d", ErrRetryable, resp.StatusCode)
	default:
		return fmt.Errorf("%w: edgegap stop: HTTP %d", ErrPermanent, resp.StatusCode)
	}
}

// waitStopped is the read-back that turns "the provider accepted a stop request" into
// "the instance is not running".
//
// The decision is on `running`, not on `current_status`, and that is measured rather than
// assumed: a live probe on 2026-11-01 saw `Status.READY -> Status.TERMINATING` with
// running=false from the first TERMINATING sample, i.e. the status string lags the fact.
// Waiting for a literal "TERMINATED" would add a deadline race for no extra truth.
//
// It returns ErrRetryable when the budget runs out with the instance still up: the
// caller records that and tries again, because an instance nobody confirmed stopped is
// a recurring charge, not an eventual inconsistency.
func (e *Edgegap) waitStopped(ctx context.Context, allocationID string) error {
	deadline := e.now().Add(e.stopWait)
	lastStatus := ""
	for {
		status, gone, err := e.status(ctx, allocationID)
		if gone {
			return nil
		}
		if err != nil {
			if !errors.Is(err, ErrRetryable) {
				// Credentials or a URL problem: waiting will not change the answer, and a
				// permanent error is what the caller must surface.
				return err
			}
			// A transient read must not abandon a deployment that is very likely still up.
		} else {
			if !status.Running {
				return nil
			}
			lastStatus = status.CurrentStatus
		}

		if !e.now().Before(deadline) {
			return fmt.Errorf("%w: edgegap deployment %s is still running %s after the stop request (status %q)",
				ErrRetryable, allocationID, e.stopWait, lastStatus)
		}
		if err := sleep(ctx, e.stopPoll); err != nil {
			return err
		}
	}
}

// deploy asks for an instance and returns the request id.
func (e *Edgegap) deploy(ctx context.Context, req Request) (string, error) {
	body := map[string]any{
		"app_name": e.app,
		"tags":     []string{"hape"},
		// The DS runtime contract (docs/ds-runtime-contract.md). The names here are the
		// contract, not an implementation detail of this adapter: a UE5 server reads the same
		// keys. BACKEND_BASE_URL and BACKEND_SERVICE_TOKEN are deployment-level (they belong
		// to the app version's environment), everything else is per match.
		"env_vars": runtimeEnv(req, e.portNum),
	}
	if e.version != "" {
		body["version_name"] = e.version
	}
	// Placement: Edgegap refuses a deployment that carries no hint at all
	// ("You must specify at least ip_list, geo_ip_list, a valid location or use
	// filters"). Player IPs are the better hint - they pick an edge near the actual
	// players - and the configured centroid is the fallback.
	switch {
	case len(req.IPs) > 0:
		body["ip_list"] = req.IPs
	case e.latitude != nil && e.longitude != nil:
		body["location"] = map[string]any{
			"latitude":  *e.latitude,
			"longitude": *e.longitude,
		}
	default:
		return "", fmt.Errorf("%w: edgegap needs player IPs (Request.IPs) or a configured "+
			"location (BACKEND_EDGEGAP_LATITUDE/LONGITUDE)", ErrPermanent)
	}

	raw, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("%w: marshal deploy body: %v", ErrPermanent, err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, e.baseURL+"/v1/deploy", bytes.NewReader(raw))
	if err != nil {
		return "", fmt.Errorf("%w: build deploy request: %v", ErrPermanent, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	e.authorize(httpReq)

	resp, err := e.client.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("%w: edgegap deploy: %v", ErrRetryable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("%w: read deploy response: %v", ErrRetryable, err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", e.errorFor(resp.StatusCode, "deploy", payload)
	}

	var decoded struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("%w: decode deploy response: %v", ErrPermanent, err)
	}
	if decoded.RequestID == "" {
		return "", fmt.Errorf("%w: edgegap deploy returned no request_id", ErrPermanent)
	}
	return decoded.RequestID, nil
}

// deploymentStatus is the subset of /v1/status/{id} this adapter reads.
type deploymentStatus struct {
	RequestID     string                 `json:"request_id"`
	CurrentStatus string                 `json:"current_status"`
	Running       bool                   `json:"running"`
	PublicIP      string                 `json:"public_ip"`
	FQDN          string                 `json:"fqdn"`
	Error         bool                   `json:"error"`
	ErrorDetail   string                 `json:"error_detail"`
	Ports         map[string]portMapping `json:"ports"`
}

type portMapping struct {
	Internal int    `json:"internal"`
	External int    `json:"external"`
	Protocol string `json:"protocol"`
	Name     string `json:"name"`
}

// status reads one deployment. gone is true when the provider no longer knows the
// deployment at all, which for a release is success and for an allocation is failure -
// so it is reported as a fact rather than folded into an error.
func (e *Edgegap) status(ctx context.Context, requestID string) (deploymentStatus, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/v1/status/"+requestID, nil)
	if err != nil {
		return deploymentStatus{}, false, fmt.Errorf("%w: build status request: %v", ErrPermanent, err)
	}
	e.authorize(req)

	resp, err := e.client.Do(req)
	if err != nil {
		return deploymentStatus{}, false, fmt.Errorf("%w: edgegap status: %v", ErrRetryable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return deploymentStatus{}, false, fmt.Errorf("%w: read status response: %v", ErrRetryable, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		// The provider has forgotten the deployment: for a release that is the goal.
		return deploymentStatus{}, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return deploymentStatus{}, false, e.errorFor(resp.StatusCode, "status", payload)
	}

	var status deploymentStatus
	if err := json.Unmarshal(payload, &status); err != nil {
		return deploymentStatus{}, false, fmt.Errorf("%w: decode status response: %v", ErrPermanent, err)
	}
	return status, false, nil
}

// allocationFrom picks the game port and builds the address we hand to the client.
func (e *Edgegap) allocationFrom(requestID string, status deploymentStatus) (Allocation, error) {
	port, ok := e.gamePort(status.Ports)
	if !ok {
		return Allocation{}, fmt.Errorf("%w: edgegap deployment %s exposes no %s/%d port (ports: %v)",
			ErrPermanent, requestID, e.portProto, e.portNum, portNames(status.Ports))
	}
	address := firstNonEmpty(status.PublicIP, status.FQDN)
	if address == "" {
		return Allocation{}, fmt.Errorf("%w: edgegap deployment %s has no address", ErrPermanent, requestID)
	}
	return Allocation{
		ID:        requestID,
		Provider:  "edgegap",
		Address:   address,
		Port:      port,
		Token:     requestID, // the DS learns its own id from the injected env
		ExpiresAt: e.now().Add(time.Hour),
	}, nil
}

// gamePort finds the external port of the game port, tolerating the ways a provider
// may label it (by name, by internal number, or just "the only UDP port").
func (e *Edgegap) gamePort(ports map[string]portMapping) (int, bool) {
	for _, p := range ports {
		if p.Name == e.portName && p.External != 0 {
			return p.External, true
		}
	}
	for _, p := range ports {
		if p.Internal == e.portNum && strings.EqualFold(p.Protocol, e.portProto) && p.External != 0 {
			return p.External, true
		}
	}
	for _, p := range ports {
		if strings.EqualFold(p.Protocol, e.portProto) && p.External != 0 {
			return p.External, true
		}
	}
	for _, p := range ports {
		if p.External != 0 {
			return p.External, true
		}
	}
	return 0, false
}

// errorFor classifies a non-2xx answer. Retryable means "the same call may work in a
// moment"; permanent means "retrying will burn money and fail the same way".
func (e *Edgegap) errorFor(status int, op string, payload []byte) error {
	detail := strings.TrimSpace(string(payload))
	if len(detail) > 300 {
		detail = detail[:300]
	}
	// A 403 is two different things in practice: a rejected token (permanent) and a
	// plan that allows fewer concurrent deployments than we want (capacity, and it
	// clears when the other deployment stops). Calling both "token rejected" sends
	// operators to the wrong console page, so the body decides.
	if status == http.StatusForbidden && isCapacityMessage(detail) {
		return fmt.Errorf("%w: edgegap %s: no deployment capacity (HTTP 403): %s", ErrRetryable, op, detail)
	}

	switch {
	case status == http.StatusTooManyRequests || status >= 500:
		return fmt.Errorf("%w: edgegap %s: HTTP %d: %s", ErrRetryable, op, status, detail)
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return fmt.Errorf("%w: edgegap %s: token rejected (HTTP %d): %s", ErrPermanent, op, status, detail)
	case status == http.StatusNotFound:
		return fmt.Errorf("%w: edgegap %s: not found (HTTP 404): %s", ErrPermanent, op, detail)
	default:
		return fmt.Errorf("%w: edgegap %s: HTTP %d: %s", ErrPermanent, op, status, detail)
	}
}

// isCapacityMessage spots a quota/concurrency refusal, which is retryable once the
// other deployment stops, as opposed to a rejected credential.
func isCapacityMessage(detail string) bool {
	lower := strings.ToLower(detail)
	for _, needle := range []string{"active deployment", "reached this limit", "upgrade your tier", "quota"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func (e *Edgegap) authorize(req *http.Request) {
	// Edgegap takes the token verbatim in Authorization, not as a Bearer token.
	req.Header.Set("Authorization", "Token "+e.token)
	req.Header.Set("Accept", "application/json")
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func portNames(ports map[string]portMapping) []string {
	out := make([]string, 0, len(ports))
	for key, p := range ports {
		out = append(out, key+":"+p.Name+"/"+strconv.Itoa(p.External))
	}
	return out
}

// runtimeEnv renders the per-match half of the DS runtime contract.
//
// The session id is BATTLE_SESSION_ID, not SESSION_ID: the contract name is what the DS
// reads, and a mismatch here is invisible until the first real deployment (the reference
// server refuses to start with "missing required environment"). PLAYERS carries the roster
// as JSON so the reference server can report without a game client.
func runtimeEnv(req Request, port int) []map[string]any {
	env := []map[string]any{
		{"key": "BATTLE_SESSION_ID", "value": req.SessionID},
		{"key": "MATCH_ID", "value": req.MatchID},
		{"key": "DS_PORT", "value": strconv.Itoa(port)},
		// The DS authenticates its report with this, not with the shared service token.
		// Hidden so it does not surface in the provider's UI.
		{"key": "RUNTIME_TOKEN", "value": req.RuntimeToken, "is_hidden": true},
	}
	if len(req.Players) > 0 {
		if raw, err := json.Marshal(req.Players); err == nil {
			env = append(env, map[string]any{"key": "PLAYERS", "value": string(raw)})
		}
	}
	if req.Map != "" {
		env = append(env, map[string]any{"key": "MAP", "value": req.Map})
	}
	if req.Queue != "" {
		env = append(env, map[string]any{"key": "QUEUE", "value": req.Queue})
	}
	return env
}
