// Package platform assembles the domain services into one process.
//
// v0 is a modular monolith on purpose: a small team gets one `go run`, one
// test target and one deployment. The seams are real — every service keeps its
// own package, store interface and contract ops — so extracting one later is a
// main.go and a deployment, not a rewrite (ADR 0005).
package platform

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"gamebackend/pkg/api"
	"gamebackend/pkg/battlealloc"
	"gamebackend/pkg/httpx"
	"gamebackend/pkg/logx"
	"gamebackend/pkg/pg"
	"gamebackend/services/account"
	"gamebackend/services/economy"
	"gamebackend/services/matchmaking"
	"gamebackend/services/progress"
	"gamebackend/services/rank"
	"gamebackend/services/session"
	"gamebackend/services/team"
)

// Config is everything the assembly needs. It mirrors pkg/config.Config but
// stays independent so tests can build a platform without touching the
// environment.
type Config struct {
	Env          string
	LogLevel     string
	DevAuth      bool
	ServiceToken string
	Allocator    string
	Logger       *slog.Logger
	// Authenticator overrides the development authenticator. Production must
	// inject one: DevAuth is a local-development shim that trusts a bearer
	// string, and a server that silently falls back to it is worse than one
	// that refuses to start.
	Authenticator httpx.Authenticator
	// DatabaseURL, when set, switches the economy store to PostgreSQL. Empty
	// keeps the in-memory store, which is what tests and a bare `go run` use.
	//
	// M1 lands one domain at a time on purpose: a half-migrated backend that
	// says which store it is running is easier to reason about than a big bang.
	DatabaseURL string
	// ReconcileInterval enables the stale-session sweep. Zero disables it.
	ReconcileInterval time.Duration
	// SessionStaleAfter is the allocating/ready age at which a session is
	// declared dead. Defaults to 5 minutes when unset.
	SessionStaleAfter time.Duration
}

// Platform is the wired application.
type Platform struct {
	log               *slog.Logger
	router            *httpx.Router
	alloc             battlealloc.Allocator
	pool              *pgxpool.Pool
	reconcileInterval time.Duration
	sessionStaleAfter time.Duration

	Account     *account.Service
	Team        *team.Service
	Matchmaking *matchmaking.Service
	Session     *session.Service
	Progress    *progress.Service
	Economy     *economy.Service
	Rank        *rank.Service
}

// New wires every service and mounts its operations on one router.
func New(cfg Config) (*Platform, error) {
	log := cfg.Logger
	if log == nil {
		log = logx.New("platform", cfg.Env, cfg.LogLevel)
	}
	if cfg.Env == "prod" && cfg.DevAuth {
		return nil, fmt.Errorf("platform: refusing to start in prod with dev auth")
	}

	// Two distinct mistakes, two distinct errors: asking for non-dev auth and
	// giving nothing to replace it with, versus asking for prod plus dev auth.
	auth := cfg.Authenticator
	if auth == nil {
		if !cfg.DevAuth {
			return nil, fmt.Errorf("platform: dev auth is disabled and no production " +
				"Authenticator was injected (see ADR 0001 / M3)")
		}
		auth = httpx.NewDevAuth(cfg.ServiceToken)
	}

	router := httpx.NewRouter("platform", log, auth)
	alloc := battlealloc.New(cfg.Allocator)

	staleAfter := cfg.SessionStaleAfter
	if staleAfter <= 0 {
		staleAfter = 5 * time.Minute
	}
	p := &Platform{
		log:               log,
		router:            router,
		alloc:             alloc,
		reconcileInterval: cfg.ReconcileInterval,
		sessionStaleAfter: staleAfter,
	}
	// Stores are chosen per domain, and the choice is logged so nobody has to
	// guess which half of the backend survives a restart.
	//
	// Every domain has a Store seam, and DatabaseURL switches all seven at once
	// (M1, spec 0002). Half-migrated state is possible in principle, but a
	// backend that says which store each domain runs on is easier to trust than
	// one that silently mixes.
	var (
		accountStore     account.Store
		teamStore        team.Store
		matchmakingStore matchmaking.Store
		sessionStore     session.Store
		progressStore    progress.Store
		economyStore     economy.Store
		rankStore        rank.Store
	)
	if cfg.DatabaseURL != "" {
		pool, err := pg.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			return nil, fmt.Errorf("platform: open database: %w", err)
		}
		p.pool = pool
		accountStore = account.NewPG(pool)
		teamStore = team.NewPG(pool)
		matchmakingStore = matchmaking.NewPG(pool)
		sessionStore = session.NewPG(pool)
		progressStore = progress.NewPG(pool)
		economyStore = economy.NewPG(pool)
		rankStore = rank.NewPG(pool)
	} else {
		accountStore = account.NewMemory()
		teamStore = team.NewMemory()
		matchmakingStore = matchmaking.NewMemory()
		sessionStore = session.NewMemory()
		progressStore = progress.NewMemory()
		economyStore = economy.NewMemory()
		rankStore = rank.NewMemory()
	}
	store := "memory"
	if cfg.DatabaseURL != "" {
		store = "postgres"
	}
	for _, domain := range []string{"account", "team", "matchmaking", "session", "progress", "economy", "rank"} {
		log.Info("store selected", "domain", domain, "store", store)
	}
	p.Account = account.NewWithStore(log.With("domain", "account"), accountStore)
	p.Team = team.NewWithStore(log.With("domain", "team"), teamStore)
	p.Economy = economy.NewWithStore(log.With("domain", "economy"), economyStore)
	p.Progress = progress.NewWithStore(log.With("domain", "progress"), progressStore)
	p.Rank = rank.NewWithStore(log.With("domain", "rank"), rankStore)
	p.Matchmaking = matchmaking.NewWithStore(log.With("domain", "matchmaking"), matchmakingStore, p.Rank.Rating)
	p.Session = session.NewWithStore(log.With("domain", "session"), sessionStore, alloc, &matchSink{
		log:      log.With("domain", "match_sink"),
		progress: p.Progress,
		economy:  p.Economy,
		rank:     p.Rank,
	})

	p.Account.Register(router)
	p.Team.Register(router)
	p.Matchmaking.Register(router)
	p.Session.Register(router)
	p.Progress.Register(router)
	p.Economy.Register(router)
	p.Rank.Register(router)
	router.Health(p.ready)

	return p, nil
}

// Ops returns every registered contract operation, in registration order.
func (p *Platform) Ops() []api.Op { return p.router.Ops() }

// Handler is the HTTP entry point.
func (p *Platform) Handler() http.Handler { return p.router }

// Logger is the process logger.
func (p *Platform) Logger() *slog.Logger { return p.log }

// Allocator is the battle allocator in use.
func (p *Platform) Allocator() battlealloc.Allocator { return p.alloc }

// Close releases resources: the database pool, once a store uses one.
func (p *Platform) Close() error {
	if p.pool != nil {
		p.pool.Close()
		p.pool = nil
	}
	return nil
}

// ReconcileInterval reports the configured sweep period (0 = disabled). Useful
// for startup logs and tests.
func (p *Platform) ReconcileInterval() time.Duration { return p.reconcileInterval }

// RunReconciler sweeps stale sessions until ctx is cancelled. It returns
// immediately when the interval is zero, so callers can start it
// unconditionally.
//
// A failed sweep is logged and retried on the next tick: the reconciler is a
// safety net, and a database blip must not take the process down with it.
func (p *Platform) RunReconciler(ctx context.Context) {
	if p.reconcileInterval <= 0 {
		return
	}
	p.log.Info("reconciler started", "interval", p.reconcileInterval, "stale_after", p.sessionStaleAfter)

	ticker := time.NewTicker(p.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			p.log.Info("reconciler stopped")
			return
		case now := <-ticker.C:
			expired, err := p.Session.ExpireStale(ctx, now, p.sessionStaleAfter)
			if err != nil {
				p.log.Error("reconcile sweep failed", "err", err)
				continue
			}
			if len(expired) > 0 {
				p.log.Warn("expired stale sessions", "count", len(expired))
			}
		}
	}
}

func (p *Platform) ready(context.Context) error { return nil }
