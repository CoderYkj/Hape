package platform_test

import (
	"io"
	"log/slog"
	"strings"
	"testing"

	"gamebackend/pkg/api"
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
