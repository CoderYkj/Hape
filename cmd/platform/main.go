// Command platform runs every backend service in one process.
//
// Local dev: `go run ./cmd/platform` then http://localhost:8080/healthz
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gamebackend/pkg/config"
	"gamebackend/pkg/httpx"
	"gamebackend/platform"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "platform: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg := config.Load("platform", ":8080")
	if err := cfg.Validate(); err != nil {
		return err
	}

	app, err := platform.New(platform.Config{
		Env:          cfg.Env,
		LogLevel:     cfg.LogLevel,
		DevAuth:      cfg.DevAuth,
		ServiceToken: cfg.ServiceToken,
		Allocator:    cfg.Allocator,
	})
	if err != nil {
		return err
	}
	defer func() { _ = app.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = httpx.Run(ctx, app.Logger(), cfg.Addr, app.Handler())
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
