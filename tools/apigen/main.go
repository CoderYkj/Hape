// Command apigen renders the OpenAPI documents from the Go contract.
//
//	go run ./tools/apigen            # write api/openapi/*.json
//	go run ./tools/apigen -check    # fail if the checked-in files are stale
//
// The -check mode is part of `verify`: a contract change without regenerated
// documents is a build failure, not a review comment.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"gamebackend/pkg/api"
	"gamebackend/platform"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "apigen: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", filepath.Join("api", "openapi"), "output directory")
	check := flag.Bool("check", false, "exit 1 when the checked-in documents are stale")
	flag.Parse()

	app, err := platform.New(platform.Config{
		Env:     "dev",
		DevAuth: true,
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		return err
	}

	ops := app.Ops()
	if err := api.Validate(ops); err != nil {
		return fmt.Errorf("contract is invalid: %w", err)
	}

	stale := 0
	for _, service := range api.Services(ops) {
		doc := api.BuildDocument(service+" service", api.ForService(ops, service))
		body, err := doc.Marshal()
		if err != nil {
			return err
		}
		path := filepath.Join(*dir, service+".json")

		if *check {
			existing, readErr := os.ReadFile(path)
			switch {
			case readErr != nil:
				fmt.Fprintf(os.Stderr, "STALE %s: %v (run: go run ./tools/apigen)\n", path, readErr)
				stale++
			case !bytes.Equal(existing, body):
				fmt.Fprintf(os.Stderr, "STALE %s: regenerate with `go run ./tools/apigen`\n", path)
				stale++
			}
			continue
		}
		if err := os.MkdirAll(*dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s (%d ops)\n", path, len(api.ForService(ops, service)))
	}
	if stale > 0 {
		return fmt.Errorf("%d stale OpenAPI document(s)", stale)
	}
	return nil
}
