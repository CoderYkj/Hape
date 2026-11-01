package config_test

import (
	"strings"
	"testing"
	"time"

	"gamebackend/pkg/config"
)

// clearEnv pins every variable Load reads, so the test result does not depend on
// the developer's shell.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"BACKEND_ENV",
		"BACKEND_ADDR",
		"LOG_LEVEL",
		"BACKEND_DEV_AUTH",
		"BACKEND_SERVICE_TOKEN",
		"BACKEND_SERVICE_CREDENTIALS",
		"BACKEND_JWT_KEY",
		"BACKEND_ALLOCATOR",
		"BACKEND_DATABASE_URL",
		"BACKEND_REGIONS",
		"BACKEND_RECONCILE_INTERVAL",
		"BACKEND_SESSION_STALE_AFTER",
	} {
		t.Setenv(key, "")
	}
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	cfg, err := config.Load("platform", ":8080")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Service != "platform" {
		t.Fatalf("service = %q, want platform", cfg.Service)
	}
	if cfg.Env != "dev" || cfg.Addr != ":8080" || cfg.LogLevel != "info" {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
	if !cfg.DevAuth || cfg.Allocator != "local" {
		t.Fatalf("dev auth / allocator defaults are wrong: %+v", cfg)
	}
	// The sweep is opt-in: a plain `go run` must not start failing sessions.
	if cfg.ReconcileInterval != 0 {
		t.Fatalf("ReconcileInterval = %s, want 0 (disabled by default)", cfg.ReconcileInterval)
	}
	if cfg.SessionStaleAfter != 5*time.Minute {
		t.Fatalf("SessionStaleAfter = %s, want 5m", cfg.SessionStaleAfter)
	}
}

func TestLoadParsesDurations(t *testing.T) {
	clearEnv(t)
	t.Setenv("BACKEND_RECONCILE_INTERVAL", "30s")
	t.Setenv("BACKEND_SESSION_STALE_AFTER", "90s")

	cfg, err := config.Load("platform", ":8080")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ReconcileInterval != 30*time.Second {
		t.Fatalf("ReconcileInterval = %s, want 30s", cfg.ReconcileInterval)
	}
	if cfg.SessionStaleAfter != 90*time.Second {
		t.Fatalf("SessionStaleAfter = %s, want 90s", cfg.SessionStaleAfter)
	}
}

// TestLoadRejectsAMalformedDuration pins the decision that a typo is an error:
// silently falling back would disable the sweep without anyone noticing.
func TestLoadRejectsAMalformedDuration(t *testing.T) {
	clearEnv(t)
	t.Setenv("BACKEND_RECONCILE_INTERVAL", "every 30 seconds")

	_, err := config.Load("platform", ":8080")
	if err == nil {
		t.Fatal("Load accepted a malformed duration")
	}
	if !strings.Contains(err.Error(), "BACKEND_RECONCILE_INTERVAL") {
		t.Fatalf("error = %v, want it to name the offending variable", err)
	}
}

func TestValidateRejectsProdWithDevAuth(t *testing.T) {
	cfg := config.Config{Env: "prod", DevAuth: true, SessionStaleAfter: time.Minute}
	if err := cfg.Validate(); err == nil {
		t.Fatal("prod + dev auth must be refused")
	}
}

func TestValidateRejectsUnknownEnv(t *testing.T) {
	cfg := config.Config{Env: "production", DevAuth: true, SessionStaleAfter: time.Minute}
	if err := cfg.Validate(); err == nil {
		t.Fatal("an unknown BACKEND_ENV must be refused")
	}
}

func TestValidateRequiresATokenWhenDevAuthIsOff(t *testing.T) {
	cfg := config.Config{Env: "dev", DevAuth: false, SessionStaleAfter: time.Minute}
	if err := cfg.Validate(); err == nil {
		t.Fatal("dev auth off without a service token must be refused")
	}
}

// TestValidateRequiresAPositiveWindow guards the reconciler: a zero window
// would mark every session failed on the first tick.
func TestValidateRequiresAPositiveWindow(t *testing.T) {
	cfg := config.Config{
		Env:               "dev",
		DevAuth:           true,
		ReconcileInterval: 30 * time.Second,
		SessionStaleAfter: 0,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("enabled sweep with a zero stale window must be refused")
	}
}

// TestValidateRefusesBothCredentialVariables pins the rule that makes a rotation
// unambiguous: during a roll you add a second entry to BACKEND_SERVICE_CREDENTIALS. A
// leftover BACKEND_SERVICE_TOKEN would keep accepting a credential the operator
// believes retired, so the two are refused together instead of merged.
func TestValidateRefusesBothCredentialVariables(t *testing.T) {
	clearEnv(t)
	t.Setenv("BACKEND_ENV", "staging")
	t.Setenv("BACKEND_DEV_AUTH", "false")
	t.Setenv("BACKEND_JWT_KEY", "key")
	t.Setenv("BACKEND_SERVICE_TOKEN", "legacy-secret")
	t.Setenv("BACKEND_SERVICE_CREDENTIALS", "nakama=secret")

	cfg, err := config.Load("platform", ":8080")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = cfg.Validate()
	if err == nil {
		t.Fatal("both credential variables were accepted")
	}
	if !strings.Contains(err.Error(), "not both") {
		t.Fatalf("error = %v, want it to say the two are exclusive", err)
	}
}

// TestValidateAcceptsACredentialSetAlone is the other half: a rotatable set satisfies
// the same requirement the single token used to.
func TestValidateAcceptsACredentialSetAlone(t *testing.T) {
	clearEnv(t)
	t.Setenv("BACKEND_ENV", "staging")
	t.Setenv("BACKEND_DEV_AUTH", "false")
	t.Setenv("BACKEND_JWT_KEY", "key")
	t.Setenv("BACKEND_SERVICE_CREDENTIALS", "nakama=one,worker=two")

	cfg, err := config.Load("platform", ":8080")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.ServiceCredentialsRaw != "nakama=one,worker=two" {
		t.Fatalf("raw credentials = %q", cfg.ServiceCredentialsRaw)
	}
}

// TestValidateRequiresSomeServiceCredential keeps the migration honest: turning dev auth
// off without either variable is still a startup failure.
func TestValidateRequiresSomeServiceCredential(t *testing.T) {
	clearEnv(t)
	t.Setenv("BACKEND_ENV", "staging")
	t.Setenv("BACKEND_DEV_AUTH", "false")
	t.Setenv("BACKEND_JWT_KEY", "key")

	cfg, err := config.Load("platform", ":8080")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("dev auth off with no service credential was accepted")
	}
}
