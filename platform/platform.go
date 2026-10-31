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

	"gamebackend/pkg/api"
	"gamebackend/pkg/battlealloc"
	"gamebackend/pkg/httpx"
	"gamebackend/pkg/logx"
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
}

// Platform is the wired application.
type Platform struct {
	log    *slog.Logger
	router *httpx.Router
	alloc  battlealloc.Allocator

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

	router := httpx.NewRouter("platform", log, httpx.NewDevAuth(cfg.ServiceToken))
	alloc := battlealloc.New(cfg.Allocator)

	p := &Platform{log: log, router: router, alloc: alloc}
	p.Account = account.New(log.With("domain", "account"))
	p.Team = team.New(log.With("domain", "team"))
	p.Progress = progress.New(log.With("domain", "progress"))
	p.Economy = economy.New(log.With("domain", "economy"))
	p.Rank = rank.New(log.With("domain", "rank"))
	p.Matchmaking = matchmaking.New(log.With("domain", "matchmaking"), p.Rank.Rating)
	p.Session = session.New(log.With("domain", "session"), alloc, &matchSink{
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

// Close releases resources. v0 holds none; the store layer will need it.
func (p *Platform) Close() error { return nil }

func (p *Platform) ready(context.Context) error { return nil }
