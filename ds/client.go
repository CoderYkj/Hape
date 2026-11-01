package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// PlayerLine is one entry of the authoritative battle report.
//
// The response schema lives in services/session; this struct is the DS-side copy of
// it. The field names are the wire contract, not an internal detail.
type PlayerLine struct {
	PlayerID string `json:"player_id"`
	Won      bool   `json:"won"`
	Score    int64  `json:"score"`
	Kills    int64  `json:"kills"`
	Deaths   int64  `json:"deaths"`
	Damage   int64  `json:"damage"`
}

// Report is the body of POST /v1/session/sessions/{id}/results.
type Report struct {
	ResultID        string       `json:"result_id"`
	DurationSeconds int64        `json:"duration_seconds"`
	Players         []PlayerLine `json:"players"`
}

// Client speaks the two DS-only endpoints. Both need the shared service token
// (proves a trusted process) and the per-session token (proves this match).
type Client struct {
	BaseURL      string
	ServiceToken string
	RuntimeToken string
	HTTP         *http.Client
	Logf         func(format string, args ...any)

	// Attempts and Backoff shape the retry loop. Zero values mean "4 attempts,
	// starting at one second", which is what the drill wants.
	Attempts int
	Backoff  time.Duration
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 10 * time.Second}
}

// ReportReady announces the listening address. Retried on transport errors and 5xx;
// a 412 means the session is not in a state that accepts ready (finished or failed),
// and no amount of retrying changes that.
func (c *Client) ReportReady(ctx context.Context, sessionID, address string, port int) error {
	_, _, err := c.post(ctx, "/v1/session/sessions/"+sessionID+"/ready",
		map[string]any{"address": address, "port": port},
		func(status int) bool { return status >= 500 })
	return err
}

// SubmitResult delivers the battle report. Returns applied=false when the backend had
// already accepted this result id, which is a success, not a duplicate payment.
func (c *Client) SubmitResult(ctx context.Context, sessionID string, report Report) (bool, error) {
	body, _, err := c.post(ctx, "/v1/session/sessions/"+sessionID+"/results", report,
		func(status int) bool {
			// 412 is retried on purpose: the session may still be moving between
			// states on a busy backend. 400/401/409 are answers, not hiccups.
			return status == http.StatusPreconditionFailed || status >= 500
		})
	if err != nil {
		return false, err
	}
	var decoded struct {
		Applied bool `json:"applied"`
	}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &decoded); err != nil {
			return false, fmt.Errorf("response was not JSON: %s", body)
		}
	}
	return decoded.Applied, nil
}

// post sends and retries, returning the body of the last successful response.
func (c *Client) post(ctx context.Context, path string, payload any,
	retry func(status int) bool) ([]byte, int, error) {

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, 0, err
	}
	attempts := c.Attempts
	if attempts <= 0 {
		attempts = 4
	}
	backoff := c.Backoff
	if backoff <= 0 {
		backoff = time.Second
	}

	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+path, bytes.NewReader(raw))
		if err != nil {
			return nil, 0, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+c.ServiceToken)
		req.Header.Set("X-Session-Token", c.RuntimeToken)

		resp, err := c.http().Do(req)
		status := 0
		var body []byte
		if err == nil {
			body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			status = resp.StatusCode
			if status >= 200 && status < 300 {
				return body, status, nil
			}
		}

		retryable := err != nil || retry(status)
		lastErr = fmt.Errorf("POST %s: status %d body %s", path, status, body)
		if err != nil {
			lastErr = fmt.Errorf("POST %s: %w", path, err)
		}
		if !retryable {
			return body, status, lastErr
		}
		if attempt == attempts {
			break
		}
		c.logf("ds: attempt %d/%d failed (%v), retrying in %s", attempt, attempts, lastErr, backoff)
		select {
		case <-ctx.Done():
			return body, status, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
	return nil, 0, fmt.Errorf("giving up after %d attempts: %w", attempts, lastErr)
}
