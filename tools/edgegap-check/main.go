// Command edgegap-check deploys one dedicated server through Edgegap and stops it
// again immediately: the smallest possible probe that the adapter, the token, the
// application and the port mapping all agree with reality.
//
// It spends a few seconds of instance time, so it is a tool and not a test.
//
//	EDGEGAP_TOKEN=... EDGEGAP_APP=Hape EDGEGAP_VERSION=demo-app-lyra \
//	  go run ./tools/edgegap-check
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"gamebackend/pkg/battlealloc"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "edgegap-check: %v\n", err)
		os.Exit(1)
	}
}

// envFloat reads an optional float flag default from the environment.
func envFloat(key string, fallback float64) float64 {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return value
}

func run() error {
	token := flag.String("token", os.Getenv("EDGEGAP_TOKEN"), "Edgegap API token")
	app := flag.String("app", os.Getenv("EDGEGAP_APP"), "Edgegap application name")
	version := flag.String("version", os.Getenv("EDGEGAP_VERSION"), "Edgegap application version (empty = latest)")
	timeout := flag.Duration("timeout", 4*time.Minute, "how long to wait for the instance to be ready")
	lat := flag.Float64("lat", envFloat("EDGEGAP_LAT", 0), "location latitude (fallback when no player IPs)")
	lon := flag.Float64("lon", envFloat("EDGEGAP_LON", 0), "location longitude")
	ipHint := flag.String("ip", os.Getenv("EDGEGAP_IP"), "player IP to place the instance near (preferred over lat/lon)")
	keep := flag.Bool("keep", false, "leave the deployment running (it will be billed and auto-expire)")
	flag.Parse()

	if *token == "" || *app == "" {
		return errors.New("EDGEGAP_TOKEN and EDGEGAP_APP are required")
	}

	cfg := battlealloc.EdgegapConfig{
		Token:        *token,
		AppName:      *app,
		Version:      *version,
		ReadyTimeout: *timeout,
		PollInterval: 2 * time.Second,
	}
	if *lat != 0 && *lon != 0 {
		cfg.Latitude, cfg.Longitude = lat, lon
	}
	allocator, err := battlealloc.NewEdgegap(cfg)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	stamp := time.Now().Unix()
	sessionID := fmt.Sprintf("smoke-%d", stamp)
	fmt.Printf("allocating %s/%s for session %s (timeout %s)\n", *app, *version, sessionID, *timeout)

	started := time.Now()
	req := battlealloc.Request{SessionID: sessionID, MatchID: "smoke-match", Queue: "smoke"}
	if *ipHint != "" {
		req.IPs = []string{*ipHint}
	}
	allocation, err := allocator.Allocate(ctx, req)
	if err != nil {
		return fmt.Errorf("allocate: %w", err)
	}
	fmt.Printf("READY in %s: id=%s address=%s:%d expires=%s\n",
		time.Since(started).Round(time.Second), allocation.ID, allocation.Address,
		allocation.Port, allocation.ExpiresAt.Format(time.RFC3339))

	if *keep {
		fmt.Printf("leaving %s running (--keep)\n", allocation.ID)
		return nil
	}

	fmt.Printf("releasing %s ...\n", allocation.ID)
	if err := allocator.Release(context.Background(), allocation.ID); err != nil {
		return fmt.Errorf("release %s failed (STOP IT MANUALLY in the Edgegap console): %w", allocation.ID, err)
	}
	fmt.Println("released; nothing should be billing for this deployment")
	return nil
}
