// Package logx builds the process logger.
//
// Logs are structured JSON on stdout, one line per event, with the fields the
// dashboards rely on: service, env, request_id, op, status, duration_ms.
package logx

import (
	"log/slog"
	"os"
	"strings"
)

// New returns a JSON logger tagged with service and env.
func New(service, env, level string) *slog.Logger {
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: ParseLevel(level)})
	return slog.New(handler).With(
		slog.String("service", service),
		slog.String("env", env),
	)
}

// ParseLevel maps LOG_LEVEL onto slog levels, defaulting to info.
func ParseLevel(v string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
