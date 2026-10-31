// Command rank runs the rank service on its own.
//
// This file exists to prove the extraction path in ADR 0005: the only
// difference between "one service" and "the platform" is which Register calls
// run on the router. No domain code changes.
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
	"gamebackend/pkg/logx"
	"gamebackend/services/rank"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rank: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load("rank", ":8087")
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	// Same rule as the platform: never serve dev bearer tokens when dev auth is
	// switched off. A split service is still a service.
	if !cfg.DevAuth {
		return fmt.Errorf("rank: dev auth is disabled and no production Authenticator is wired (M3)")
	}

	log := logx.New("rank", cfg.Env, cfg.LogLevel)
	router := httpx.NewRouter("rank", log, httpx.NewDevAuth(cfg.ServiceToken))
	rank.NewWithStore(log, rank.NewMemory()).Register(router)
	router.Health(nil)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err = httpx.Run(ctx, log, cfg.Addr, router)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
