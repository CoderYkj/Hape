// Package config loads process configuration from the environment only.
//
// No config files, no flags: a container gets env vars and nothing else. Every
// knob has a safe default except secrets, which have none.
package config

import (
	"fmt"
	"os"
	"strings"
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
	Allocator    string // local | gamelift | edgegap | agones
	DatabaseURL  string // empty in v0: stores are in memory
	BatchRegions []string
}

// Load reads the environment for one process.
func Load(service, defaultAddr string) Config {
	return Config{
		Service:      service,
		Env:          envOr("BACKEND_ENV", "dev"),
		Addr:         envOr("BACKEND_ADDR", defaultAddr),
		LogLevel:     envOr("LOG_LEVEL", "info"),
		DevAuth:      envOr("BACKEND_DEV_AUTH", "true") == "true",
		ServiceToken: os.Getenv("BACKEND_SERVICE_TOKEN"),
		Allocator:    envOr("BACKEND_ALLOCATOR", "local"),
		DatabaseURL:  os.Getenv("BACKEND_DATABASE_URL"),
		BatchRegions: splitList(envOr("BACKEND_REGIONS", "local")),
	}
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
	return nil
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
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
