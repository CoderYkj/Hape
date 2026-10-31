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
	app       string
	version   string
	baseURL   string
	client    *http.Client
	poll      time.Duration
	readyWait time.Duration
	portName  string
	portProto string
	portNum   int
	now       func() time.Time
}

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
	// PortName/PortProto/PortNum identify the game port in the deployment. The
	// Edgegap app registers it as name=gameport, 7777/UDP.
	PortName  string
	PortProto string
	PortNum   int
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
		cfg.ReadyTimeout = 3 * time.Minute
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
	return &Edgegap{
		token:     cfg.Token,
		app:       cfg.AppName,
		version:   cfg.Version,
		baseURL:   strings.TrimRight(cfg.BaseURL, "/"),
		client:    cfg.Client,
		poll:      cfg.PollInterval,
		readyWait: cfg.ReadyTimeout,
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
		status, err := e.status(ctx, requestID)
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
			_ = e.Release(ctx, requestID)
			return Allocation{}, fmt.Errorf("%w: edgegap deployment failed: %s",
				ErrPermanent, firstNonEmpty(status.ErrorDetail, status.CurrentStatus))
		}

		if status.Running {
			allocation, err := e.allocationFrom(requestID, status)
			if err != nil {
				_ = e.Release(ctx, requestID)
				return Allocation{}, err
			}
			return allocation, nil
		}

		if !e.now().Before(deadline) {
			_ = e.Release(ctx, requestID)
			return Allocation{}, fmt.Errorf("%w: edgegap deployment %s not ready after %s (status %q)",
				ErrRetryable, requestID, e.readyWait, status.CurrentStatus)
		}
		if err := sleep(ctx, e.poll); err != nil {
			_ = e.Release(ctx, requestID)
			return Allocation{}, err
		}
	}
}

// Release implements Allocator. Deleting an already-deleted deployment is a
// success: release must be safe to retry.
func (e *Edgegap) Release(ctx context.Context, allocationID string) error {
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

// deploy asks for an instance and returns the request id.
func (e *Edgegap) deploy(ctx context.Context, req Request) (string, error) {
	body := map[string]any{
		"app_name": e.app,
		"tags":     []string{"hape"},
		"env_vars": []map[string]any{
			{"key": "SESSION_ID", "value": req.SessionID},
			{"key": "MATCH_ID", "value": req.MatchID},
			// The DS authenticates its report with this, not with the shared service
			// token. Hidden so it does not surface in the provider's UI.
			{"key": "RUNTIME_TOKEN", "value": req.RuntimeToken, "is_hidden": true},
		},
	}
	if e.version != "" {
		body["version_name"] = e.version
	}
	// Player IPs make Edgegap pick a nearby edge location (geo selection).
	if len(req.IPs) > 0 {
		body["ip_list"] = req.IPs
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

func (e *Edgegap) status(ctx context.Context, requestID string) (deploymentStatus, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.baseURL+"/v1/status/"+requestID, nil)
	if err != nil {
		return deploymentStatus{}, fmt.Errorf("%w: build status request: %v", ErrPermanent, err)
	}
	e.authorize(req)

	resp, err := e.client.Do(req)
	if err != nil {
		return deploymentStatus{}, fmt.Errorf("%w: edgegap status: %v", ErrRetryable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return deploymentStatus{}, fmt.Errorf("%w: read status response: %v", ErrRetryable, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return deploymentStatus{}, e.errorFor(resp.StatusCode, "status", payload)
	}

	var status deploymentStatus
	if err := json.Unmarshal(payload, &status); err != nil {
		return deploymentStatus{}, fmt.Errorf("%w: decode status response: %v", ErrPermanent, err)
	}
	return status, nil
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
