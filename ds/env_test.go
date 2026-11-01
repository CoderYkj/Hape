package main

import (
	"strings"
	"testing"
	"time"
)

func envFunc(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

func validEnv() map[string]string {
	return map[string]string{
		envBaseURL:      "http://127.0.0.1:8080/",
		envServiceToken: "dev-service",
		envSessionID:    "s_abc",
		envRuntimeToken: "runtime-token-value",
		envPlayers:      `["p1","p2"]`,
	}
}

func TestLoadConfigAppliesDefaults(t *testing.T) {
	cfg, err := LoadConfig(envFunc(validEnv()))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Trailing slash trimmed: every request path is appended to this.
	if cfg.BaseURL != "http://127.0.0.1:8080" {
		t.Fatalf("BaseURL = %q", cfg.BaseURL)
	}
	if cfg.Port != 7777 {
		t.Fatalf("Port = %d, want 7777", cfg.Port)
	}
	if cfg.Duration != 3*time.Second {
		t.Fatalf("Duration = %s, want 3s", cfg.Duration)
	}
	if cfg.ResultsDir != "Saved/Results" {
		t.Fatalf("ResultsDir = %q", cfg.ResultsDir)
	}
	if cfg.Advertise != "127.0.0.1" {
		t.Fatalf("Advertise = %q", cfg.Advertise)
	}
	if cfg.CrashBeforeReport {
		t.Fatal("CrashBeforeReport should default to false")
	}
	if len(cfg.Players) != 2 {
		t.Fatalf("Players = %v", cfg.Players)
	}
}

func TestLoadConfigNamesEveryMissingVariable(t *testing.T) {
	// The realistic failure is a provider that forgot one variable. The error has to
	// name it, otherwise the drill's output says nothing useful.
	_, err := LoadConfig(envFunc(map[string]string{}))
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{envBaseURL, envServiceToken, envSessionID, envRuntimeToken, envPlayers} {
		if !strings.Contains(err.Error(), key) {
			t.Fatalf("error does not name %s: %v", key, err)
		}
	}
}

func TestLoadConfigRejectsMalformedPlayers(t *testing.T) {
	values := validEnv()
	values[envPlayers] = "p1,p2"
	if _, err := LoadConfig(envFunc(values)); err == nil || !strings.Contains(err.Error(), envPlayers) {
		t.Fatalf("want an error naming %s, got %v", envPlayers, err)
	}

	values[envPlayers] = `["solo"]`
	if _, err := LoadConfig(envFunc(values)); err == nil {
		t.Fatal("a single player is not a match")
	}
}

func TestLoadConfigRejectsBadPort(t *testing.T) {
	values := validEnv()
	values[envPort] = "70000"
	if _, err := LoadConfig(envFunc(values)); err == nil {
		t.Fatal("expected an error for an out-of-range port")
	}
}

func TestBannerReportsPresenceAndLengthButNeverTheValue(t *testing.T) {
	values := validEnv()
	values[envRuntimeToken] = "super-secret-runtime-token"
	values[envServiceToken] = "another-secret"
	cfg, err := LoadConfig(envFunc(values))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	banner := cfg.Banner()

	if strings.Contains(banner, "super-secret-runtime-token") || strings.Contains(banner, "another-secret") {
		t.Fatalf("the banner leaked a credential: %s", banner)
	}
	// The length is what proves the injection arrived, so it must be there.
	if !strings.Contains(banner, "runtime_token=present(len=26)") {
		t.Fatalf("banner does not report the runtime token length: %s", banner)
	}
	if !strings.Contains(banner, "service_token=present(len=14)") {
		t.Fatalf("banner does not report the service token length: %s", banner)
	}
	if !strings.Contains(banner, "session=s_abc") {
		t.Fatalf("banner does not name the session: %s", banner)
	}
}
