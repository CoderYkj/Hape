// Package config loads process configuration from the environment only.
//
// No config files, no flags: a container gets env vars and nothing else. Every
// knob has a safe default except secrets, which have none.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the union of every knob the backend reads today. Growth is cheap;
// an env var that nothing reads is not a config, it is a rumour.
type Config struct {
	Service      string
	Env          string // dev | staging | prod
	Addr         string
	LogLevel     string
	DevAuth      bool
	ServiceToken string
	JWTKey       string // Nakama session encryption key; required when DevAuth is off
	JWTIssuer    string // optional `iss` check
	Allocator    string // local | gamelift | edgegap | agones
	EdgegapToken string
	EdgegapApp   string
	EdgegapVer   string
	EdgegapURL   string
	EdgegapLat   float64
	EdgegapLon   float64
	DatabaseURL  string // empty in v0: stores are in memory
	BatchRegions []string

	// ReconcileInterval is how often the stale-session sweep runs. Zero (the
	// default) disables it: a single-process dev server does not need a timer,
	// and silently failing sessions would be surprising there.
	ReconcileInterval time.Duration
	// SessionStaleAfter is how long a session may sit in allocating or ready
	// before the sweep declares it dead.
	SessionStaleAfter time.Duration
}

// Load reads the environment for one process. A malformed value is an error, not
// a silent fallback: a typo in a duration must not quietly disable the sweep.
func Load(service, defaultAddr string) (Config, error) {
	reconcile, err := duration("BACKEND_RECONCILE_INTERVAL", 0)
	if err != nil {
		return Config{}, err
	}
	staleAfter, err := duration("BACKEND_SESSION_STALE_AFTER", 5*time.Minute)
	if err != nil {
		return Config{}, err
	}
	return Config{
		Service:           service,
		Env:               envOr("BACKEND_ENV", "dev"),
		Addr:              envOr("BACKEND_ADDR", defaultAddr),
		LogLevel:          envOr("LOG_LEVEL", "info"),
		DevAuth:           envOr("BACKEND_DEV_AUTH", "true") == "true",
		ServiceToken:      os.Getenv("BACKEND_SERVICE_TOKEN"),
		JWTKey:            os.Getenv("BACKEND_JWT_KEY"),
		JWTIssuer:         os.Getenv("BACKEND_JWT_ISSUER"),
		Allocator:         envOr("BACKEND_ALLOCATOR", "local"),
		EdgegapToken:      os.Getenv("BACKEND_EDGEGAP_TOKEN"),
		EdgegapApp:        envOr("BACKEND_EDGEGAP_APP", ""),
		EdgegapVer:        os.Getenv("BACKEND_EDGEGAP_VERSION"),
		EdgegapURL:        os.Getenv("BACKEND_EDGEGAP_BASE_URL"),
		EdgegapLat:        floatOr("BACKEND_EDGEGAP_LATITUDE", 0),
		EdgegapLon:        floatOr("BACKEND_EDGEGAP_LONGITUDE", 0),
		DatabaseURL:       os.Getenv("BACKEND_DATABASE_URL"),
		BatchRegions:      splitList(envOr("BACKEND_REGIONS", "local")),
		ReconcileInterval: reconcile,
		SessionStaleAfter: staleAfter,
	}, nil
}

// IsProd reports whether this is the production environment.
func (c Config) IsProd() bool { return c.Env == "prod" }

// Validate refuses configurations that must never reach prod.
func (c Config) Validate() error {
	switch c.Env {
	case "dev", "staging", "prod":
	default:
		return fmt.Errorf("BACKEND_ENV must be dev, staging or prod, got %q", c.Env)
	}
	if c.IsProd() && c.DevAuth {
		return fmt.Errorf("BACKEND_DEV_AUTH=true is refused when BACKEND_ENV=prod")
	}
	if !c.IsProd() && !c.DevAuth && c.ServiceToken == "" {
		return fmt.Errorf("BACKEND_DEV_AUTH=false requires BACKEND_SERVICE_TOKEN")
	}
	if !c.DevAuth && c.JWTKey == "" {
		return fmt.Errorf("BACKEND_DEV_AUTH=false requires BACKEND_JWT_KEY (Nakama session encryption key)")
	}
	if c.Allocator == "edgegap" && c.EdgegapToken == "" {
		return fmt.Errorf("BACKEND_ALLOCATOR=edgegap requires BACKEND_EDGEGAP_TOKEN")
	}
	if c.Allocator == "edgegap" && c.EdgegapApp == "" {
		return fmt.Errorf("BACKEND_ALLOCATOR=edgegap requires BACKEND_EDGEGAP_APP")
	}
	if c.ReconcileInterval < 0 {
		return fmt.Errorf("BACKEND_RECONCILE_INTERVAL must not be negative, got %s", c.ReconcileInterval)
	}
	if c.ReconcileInterval > 0 && c.SessionStaleAfter <= 0 {
		return fmt.Errorf("BACKEND_SESSION_STALE_AFTER must be positive when the sweep is enabled, got %s", c.SessionStaleAfter)
	}
	if c.SessionStaleAfter < 0 {
		return fmt.Errorf("BACKEND_SESSION_STALE_AFTER must not be negative, got %s", c.SessionStaleAfter)
	}
	return nil
}

// duration parses a Go duration (300ms, 5m, 2h) out of the environment.
func duration(key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", key, err)
	}
	return parsed, nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

// floatOr parses an optional float; a malformed value falls back rather than
// failing startup, because a bad coordinate only matters when it is used.
func floatOr(key string, fallback float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func splitList(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
