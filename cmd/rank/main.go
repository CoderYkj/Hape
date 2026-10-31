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
	cfg := config.Load("rank", ":8087")
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := logx.New("rank", cfg.Env, cfg.LogLevel)
	router := httpx.NewRouter("rank", log, httpx.NewDevAuth(cfg.ServiceToken))
	rank.NewWithStore(log, rank.NewMemory()).Register(router)
	router.Health(nil)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := httpx.Run(ctx, log, cfg.Addr, router)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
