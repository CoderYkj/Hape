// Package battlealloc is the seam between "we need a match server" and
// "somebody gives us one".
//
// Consequence of the design: the session service never imports a vendor SDK.
// A managed provider (GameLift, Edgegap, ...) is one adapter behind Allocator,
// so switching providers is a deploy, not a rewrite. See
// .harness/rules/90-battle-integration.md and ADR 0003.
package battlealloc

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// ErrNotConfigured is returned by adapters that are declared but not wired yet.
var ErrNotConfigured = errors.New("battle allocator: provider not configured")

// ErrRetryable marks a failure worth retrying: rate limiting, 5xx, a network blip.
// ErrPermanent marks one where a retry would fail identically and burn quota
// (rejected token, missing application, malformed request).
var (
	ErrRetryable = errors.New("battlealloc: retryable failure")
	ErrPermanent = errors.New("battlealloc: permanent failure")
)

// Request asks for one dedicated server instance.
type Request struct {
	SessionID  string
	MatchID    string
	Queue      string
	Region     string
	Map        string
	MaxPlayers int
	// IPs are the players' addresses, used by providers that pick a nearby edge
	// location. Optional: an empty list means "provider decides".
	IPs []string
	// RuntimeToken is the per-session credential the dedicated server must present
	// when it reports ready/results. Adapters inject it into the instance
	// environment; the platform mints it, the provider only carries it.
	RuntimeToken string
	Meta         map[string]string
}

// Allocation is a granted dedicated server.
type Allocation struct {
	ID        string
	Provider  string
	Address   string
	Port      int
	Token     string // join token the DS and clients must present
	ExpiresAt time.Time
}

// Allocator grants and releases dedicated servers.
type Allocator interface {
	Provider() string
	Allocate(ctx context.Context, req Request) (Allocation, error)
	Release(ctx context.Context, allocationID string) error
}

// Local hands out a fixed loopback endpoint. It is what tests and the
// fakeds tool use, and it keeps the whole match loop runnable offline.
type Local struct {
	addr   string
	port   int
	serial atomic.Uint64
}

// NewLocal builds the offline allocator.
func NewLocal() *Local {
	return &Local{addr: "127.0.0.1", port: 7777}
}

// Provider implements Allocator.
func (l *Local) Provider() string { return "local" }

// Allocate implements Allocator.
func (l *Local) Allocate(_ context.Context, req Request) (Allocation, error) {
	if req.SessionID == "" {
		return Allocation{}, fmt.Errorf("battlealloc: session id is required")
	}
	n := l.serial.Add(1)
	return Allocation{
		ID:        fmt.Sprintf("local-%s-%d", req.SessionID, n),
		Provider:  "local",
		Address:   l.addr,
		Port:      l.port + int(n%64),
		Token:     firstNonEmptyOr("local-"+req.SessionID, req.RuntimeToken),
		ExpiresAt: time.Now().Add(30 * time.Minute),
	}, nil
}

// Release implements Allocator.
func (l *Local) Release(context.Context, string) error { return nil }

// Unconfigured is a declared-but-unwired vendor adapter. It fails loudly
// instead of silently pretending to allocate a server.
type Unconfigured struct{ Name string }

// Provider implements Allocator.
func (u Unconfigured) Provider() string { return u.Name }

// Allocate implements Allocator.
func (u Unconfigured) Allocate(context.Context, Request) (Allocation, error) {
	return Allocation{}, fmt.Errorf("%w: %s (implement Allocator in pkg/battlealloc)", ErrNotConfigured, u.Name)
}

// Release implements Allocator.
func (u Unconfigured) Release(context.Context, string) error {
	return fmt.Errorf("%w: %s", ErrNotConfigured, u.Name)
}

// New resolves a provider name into an Allocator. Unwired vendors fall through
// to Unconfigured on purpose: a typo in BACKEND_ALLOCATOR must not silently
// become "local" in staging.
func New(provider string) Allocator {
	switch provider {
	case "", "local":
		return NewLocal()
	case "gamelift", "edgegap", "agones":
		return Unconfigured{Name: provider}
	default:
		return Unconfigured{Name: provider}
	}
}

// firstNonEmptyOr prefers the caller-supplied value: the local allocator must hand
// back the same per-session token the backend stored, or the DS cannot authenticate.
func firstNonEmptyOr(fallback, preferred string) string {
	if preferred != "" {
		return preferred
	}
	return fallback
}
