// Command allocation-report prints the allocation ledger (spec 0018).
//
// It answers the two questions billing reconciliation needs from our side: which instances
// never got a confirmed release (the only ones that can still be billing), and how long each
// instance actually ran.
//
// It reads the database directly, like psql does: the ledger is an operations view, not an
// API a client should ever call.
//
//	BACKEND_DATABASE_URL=postgres://... go run ./tools/allocation-report
//	go run ./tools/allocation-report -unreleased -since 24h
//	go run ./tools/allocation-report -json | jq '.[] | select(.unreleased)'
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"gamebackend/pkg/logx"
	"gamebackend/pkg/pg"
	"gamebackend/services/session"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "allocation-report: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dsn := flag.String("dsn", os.Getenv("BACKEND_DATABASE_URL"), "PostgreSQL DSN (default: BACKEND_DATABASE_URL)")
	unreleased := flag.Bool("unreleased", false, "only allocations with no confirmed release")
	since := flag.Duration("since", 0, "only allocations newer than this (e.g. 24h, 30m)")
	limit := flag.Int("limit", 0, "maximum rows (0 = all)")
	asJSON := flag.Bool("json", false, "print JSON instead of a table")
	flag.Parse()

	if *dsn == "" {
		return fmt.Errorf("set BACKEND_DATABASE_URL or pass -dsn")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pg.Open(ctx, *dsn)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer pool.Close()

	// The store and service are the same ones the process uses, so the report cannot drift
	// from the rules the process enforces.
	svc := session.NewWithStore(logx.New("allocation-report", "dev", "warn"),
		session.NewPG(pool), nil, nil)

	query := session.AllocationQuery{UnreleasedOnly: *unreleased, Limit: *limit}
	if *since > 0 {
		query.Since = time.Now().UTC().Add(-*since)
	}
	rows, err := svc.Allocations(ctx, query, time.Now().UTC())
	if err != nil {
		return err
	}

	if *asJSON {
		encoded, err := json.MarshalIndent(rows, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	}

	printTable(rows)
	return nil
}

func printTable(rows []session.AllocationRow) {
	if len(rows) == 0 {
		fmt.Println("no allocations recorded")
		return
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "SESSION\tPROVIDER\tALLOCATION\tSTATE\tALLOCATED\tRELEASED\tSECONDS\tRETRY")
	var (
		unreleased     int
		totalSeconds   int64
		oldestUnrelSec int64
	)
	for _, row := range rows {
		released := "-"
		if row.ReleasedAt != nil {
			released = row.ReleasedAt.UTC().Format("01-02 15:04:05")
		} else {
			unreleased++
			if row.UnreleasedFor.Seconds() > float64(oldestUnrelSec) {
				oldestUnrelSec = int64(row.UnreleasedFor.Seconds())
			}
		}
		retry := "-"
		if row.ReleasePending {
			retry = fmt.Sprintf("pending x%d", row.ReleaseAttempts)
		}
		totalSeconds += row.InstanceSeconds
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n",
			row.SessionID, row.Provider, row.AllocationID, row.State,
			row.AllocatedAt.UTC().Format("01-02 15:04:05"), released,
			row.InstanceSeconds, retry)
	}
	_ = w.Flush()

	fmt.Printf("\n%d allocations, %d unreleased, %ds of instance time",
		len(rows), unreleased, totalSeconds)
	if unreleased > 0 {
		fmt.Printf(" (oldest unreleased for %ds)", oldestUnrelSec)
	}
	fmt.Println()
	if unreleased > 0 {
		fmt.Println("unreleased = the provider never confirmed the stop; these are the only rows that can still be billing")
	}
}
