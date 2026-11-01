package platform_test

import (
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"gamebackend/pkg/api"
	"gamebackend/pkg/apierr"
	"gamebackend/pkg/httpx"
	"gamebackend/platform"
)

func newApp(t *testing.T) *platform.Platform {
	t.Helper()
	app, err := platform.New(platform.Config{
		Env:       "test",
		DevAuth:   true,
		Allocator: "local",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("platform.New: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return app
}

// TestContractIsValid is the gate: a malformed operation declaration fails the
// build, not production.
func TestContractIsValid(t *testing.T) {
	app := newApp(t)
	if err := api.Validate(app.Ops()); err != nil {
		t.Fatalf("contract invalid: %v", err)
	}
}

// TestEveryContractServiceHasOps makes sure a service cannot be wired without
// declaring at least one operation (a wired-but-empty service is a silent
// deployment bug).
func TestEveryContractServiceHasOps(t *testing.T) {
	app := newApp(t)
	ops := app.Ops()
	services := api.Services(ops)
	want := []string{"account", "economy", "matchmaking", "progress", "rank", "session", "team"}

	got := strings.Join(services, ",")
	if got != strings.Join(want, ",") {
		t.Fatalf("services = %s, want %s", got, strings.Join(want, ","))
	}
	for _, service := range services {
		if len(api.ForService(ops, service)) == 0 {
			t.Fatalf("service %q has no operations", service)
		}
	}
}

// TestContractWriteOpsDeclareRetrySemantics documents the policy: every write
// either declares Idempotent (safe to replay) or explains in Note why replaying
// it would be wrong. Silence is what causes double payouts.
func TestContractWriteOpsDeclareRetrySemantics(t *testing.T) {
	app := newApp(t)
	for _, op := range app.Ops() {
		if op.Method == "GET" {
			continue
		}
		if !op.Idempotent && op.Note == "" {
			t.Errorf("%s (%s %s) is not idempotent and does not explain why in Note", op.ID, op.Method, op.Path)
		}
	}
}

// TestServiceOpsRequireServiceAuth guards the authority boundary: a
// server-to-server operation must never be reachable with a player token.
func TestServiceOpsRequireServiceAuth(t *testing.T) {
	app := newApp(t)
	for _, op := range app.Ops() {
		switch op.Service {
		case "session":
			if op.ID != "session.get" && op.Auth != api.AuthService {
				t.Errorf("%s must be service-authenticated", op.ID)
			}
		}
	}
}

// rejectAll is a stand-in for "the real authenticator, not yet written": it
// authenticates nobody, but it proves the injection point works.
type rejectAll struct{}

func (rejectAll) Player(*http.Request) (httpx.Principal, error) {
	return httpx.Principal{}, apierr.Unauthenticated("production authenticator not implemented")
}

func (rejectAll) Service(*http.Request) (httpx.Principal, error) {
	return httpx.Principal{}, apierr.Unauthenticated("production authenticator not implemented")
}

// TestPlatformRefusesToStartWithoutAnAuthenticator is the deployment guard:
// BACKEND_DEV_AUTH=false without a real authenticator must fail at startup, not
// serve dev bearer tokens in staging or prod.
func TestPlatformRefusesToStartWithoutAnAuthenticator(t *testing.T) {
	_, err := platform.New(platform.Config{
		Env:       "staging",
		DevAuth:   false,
		Allocator: "local",
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err == nil {
		t.Fatal("platform started with dev auth disabled and no authenticator injected")
	}
	// With a production authenticator built in, the refusal now names the missing
	// the missing key (whatever layer notices it first: the verifier or the issuer).
	if !strings.Contains(err.Error(), "key") {
		t.Fatalf("error = %v, want it to mention the missing key", err)
	}
}

// TestPlatformAcceptsAnInjectedAuthenticator keeps that guard from turning into
// a dead end: the seam has to actually work.
func TestPlatformAcceptsAnInjectedAuthenticator(t *testing.T) {
	app, err := platform.New(platform.Config{
		Env:           "staging",
		DevAuth:       false,
		Allocator:     "local",
		Authenticator: rejectAll{},
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("platform.New with an injected authenticator: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
}
