// Package pg is the only place in this repository that imports a PostgreSQL
// driver. Everything else talks to a domain Store interface.
//
// Why a wrapper instead of using pgx directly in every store:
//
//   - one place for connection settings, so a pool policy is not re-invented
//     (and mis-invented) per service;
//   - one place for error translation, so "unique violation" becomes a domain
//     answer ("this idempotency key was already applied") instead of a driver
//     type leaking into a handler;
//   - one place to swap the driver if it ever has to be swapped.
//
// See ADR 0006 for the dependency decision.
package pg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the slice of pgx the stores need. It is satisfied by both *pgxpool.Pool
// and pgx.Tx, which is what makes "run this against a transaction" possible
// without a second implementation of every store.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Open parses the DSN, applies the pool policy and proves the connection works.
//
// Failing here is the point: a process that cannot reach its database should die
// at startup, not serve errors it cannot explain.
func Open(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	if dsn == "" {
		return nil, errors.New("pg: empty DSN")
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("pg: parse DSN: %w", err)
	}

	// Defaults sized for one small process, not for a benchmark. 10 connections
	// is plenty for a team-sized game backend; raising it is a decision that
	// needs a measurement, not a guess.
	if cfg.MaxConns < 4 {
		cfg.MaxConns = 10
	}
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pg: create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("pg: ping: %w", err)
	}
	return pool, nil
}

// InTx runs fn inside a transaction, committing on nil and rolling back on any
// error (including a panic, which still rolls back because the rollback is
// deferred before fn runs).
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(tx pgx.Tx) error) error {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("pg: begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			// Rolling back a transaction that already aborted is harmless.
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg: commit: %w", err)
	}
	committed = true
	return nil
}
