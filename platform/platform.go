// Package platform assembles the domain services into one process.
//
// v0 is a modular monolith on purpose: a small team gets one `go run`, one
// test target and one deployment. The seams are real — every service keeps its
// own package, store interface and contract ops — so extracting one later is a
// main.go and a deployment, not a rewrite (ADR 0005).
package platform

import (
	"context"
	"errors"
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
	// ServiceCredentials accepts several named service tokens at once. When empty, a
	// legacy single ServiceToken is wrapped as one credential named "service", so an
	// existing deployment keeps working while it migrates to a rotatable set.
	ServiceCredentials httpx.ServiceCredentials
	Allocator          string
	Logger             *slog.Logger
	// Authenticator overrides the built-in choice entirely (tests, or a deployment
	// with its own identity provider).
	Authenticator httpx.Authenticator
	// AllocatorOverride replaces the provider built from Allocator. Tests use it to
	// script a provider that fails to release; production has no reason to.
	AllocatorOverride battlealloc.Allocator
	// ObserveServiceScopes disables scope enforcement but keeps the log lines. Used to
	// validate a scope policy against real traffic before it starts refusing calls.
	ObserveServiceScopes bool
	// JWTKey is Nakama's session encryption key. Required when DevAuth is false.
	JWTKey string
	// JWTIssuer, when set, is checked against the token's `iss`.
	JWTIssuer string
	// PlayerTokenTTL is how long a backend-issued player token lives (default 1h).
	PlayerTokenTTL time.Duration
	// PlayerJWTKey signs player tokens; empty means "use JWTKey".
	PlayerJWTKey string
	// Edgegap credentials, used only when Allocator is "edgegap".
	EdgegapToken   string
	EdgegapApp     string
	EdgegapVersion string
	EdgegapBaseURL string
	// EdgegapLat/Lon are the fallback placement centroid (0,0 = unset).
	EdgegapLat float64
	EdgegapLon float64
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
	metrics           *httpx.Metrics

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

	// The override exists so a test can script "the provider will not let go" without a
	// vendor account; production always goes through the name.
	alloc := cfg.AllocatorOverride
	if alloc != nil {
		log.Info("allocator selected", "provider", alloc.Provider(), "override", true)
	} else if cfg.Allocator == "edgegap" {
		alloc = battlealloc.New(cfg.Allocator)
		edgegapCfg := battlealloc.EdgegapConfig{
			Token:   cfg.EdgegapToken,
			AppName: cfg.EdgegapApp,
			Version: cfg.EdgegapVersion,
			BaseURL: cfg.EdgegapBaseURL,
		}
		// A centroid is optional: without it, every allocation must carry player IPs.
		if cfg.EdgegapLat != 0 && cfg.EdgegapLon != 0 {
			lat, lon := cfg.EdgegapLat, cfg.EdgegapLon
			edgegapCfg.Latitude, edgegapCfg.Longitude = &lat, &lon
		}
		edgegap, err := battlealloc.NewEdgegap(edgegapCfg)
		if err != nil {
			return nil, fmt.Errorf("platform: edgegap allocator: %w", err)
		}
		alloc = edgegap
		log.Info("allocator selected", "provider", "edgegap", "app", cfg.EdgegapApp)
	} else {
		alloc = battlealloc.New(cfg.Allocator)
		log.Info("allocator selected", "provider", alloc.Provider())
	}

	staleAfter := cfg.SessionStaleAfter
	if staleAfter <= 0 {
		staleAfter = 5 * time.Minute
	}
	p := &Platform{
		log:               log,
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
	var pool *pgxpool.Pool
	if cfg.DatabaseURL != "" {
		opened, err := pg.Open(context.Background(), cfg.DatabaseURL)
		if err != nil {
			return nil, fmt.Errorf("platform: open database: %w", err)
		}
		pool = opened
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
	var playerMinter account.TokenMinter
	// Authentication comes after the stores because a real JWT authenticator needs
	// to resolve Nakama's user id to a backend player id, which is the account
	// store's job. Dev auth needs nothing.
	auth := cfg.Authenticator
	// One credential set for both authenticators: dev and production must not disagree
	// about which tokens are valid, or a rotation would be verified in the wrong mode.
	creds := cfg.ServiceCredentials
	// The legacy single-token paths are unscoped by construction. That is a property of
	// the variable, not a loophole: the only way to get least privilege is to declare
	// scopes in BACKEND_SERVICE_CREDENTIALS.
	if len(creds) == 0 && cfg.ServiceToken != "" {
		creds = httpx.ServiceCredentials{{
			Name:     httpx.LegacyServiceCredentialName,
			Token:    cfg.ServiceToken,
			Services: []string{httpx.AllServices},
		}}
	}
	if len(creds) == 0 && cfg.DevAuth {
		// Dev stays zero-configuration: `go run ./cmd/platform` and every local tool
		// share dev-service, exactly as before credentials became a set.
		creds = httpx.ServiceCredentials{{
			Name:     httpx.LegacyServiceCredentialName,
			Token:    httpx.DevServiceToken,
			Services: []string{httpx.AllServices},
		}}
	}
	// Empty is not "invalid" here: the authenticator below reports the missing
	// credential in the vocabulary of the mode it is building (a JWT verifier without a
	// service token, or dev auth's default). Anything non-empty must be well-formed.
	if len(creds) > 0 {
		if err := creds.Validate(); err != nil {
			return nil, fmt.Errorf("platform: service credentials: %w", err)
		}
	}
	if auth == nil {
		if cfg.DevAuth {
			auth = httpx.NewDevAuthWithCredentials(creds)
			log.Info("auth selected", "mode", "dev", "service_credentials", creds.Describe())
		} else {
			// A deployment that wants player tokens signed with a different key than
			// Nakama's can set BACKEND_PLAYER_JWT_KEY; by default one key signs both and
			// the iss claim keeps them apart.
			playerKey := cfg.PlayerJWTKey
			if playerKey == "" {
				playerKey = cfg.JWTKey
			}
			playerIssuer, err := httpx.NewTokenIssuer([]byte(playerKey), cfg.PlayerTokenTTL)
			if err != nil {
				return nil, fmt.Errorf("platform: player token issuer: %w", err)
			}
			// Login mints a token this backend will accept back (iss=gamebackend).
			// Without this, production would hand clients a dev token that the JWT
			// verifier rejects - the login path would be broken outside dev (ADR 0010).
			playerMinter = func(playerID string) (string, error) {
				token, _, err := playerIssuer.Issue(playerID)
				return token, err
			}
			jwtAuth, err := httpx.NewJWTAuth(httpx.JWTAuthConfig{
				SecurityKey:        []byte(cfg.JWTKey),
				PlayerKey:          []byte(playerKey),
				ServiceCredentials: creds,
				Issuer:             cfg.JWTIssuer,
				Resolve: func(ctx context.Context, externalID string) (string, error) {
					player, err := accountStore.LookupByExternalID(ctx, externalID)
					if err != nil {
						return "", err
					}
					return player.ID, nil
				},
			})
			if err != nil {
				return nil, fmt.Errorf("platform: production authenticator: %w", err)
			}
			auth = jwtAuth
			log.Info("auth selected", "mode", "jwt", "service_credentials", creds.Describe())
		}
	}
	router := httpx.NewRouter("platform", log, auth)
	if cfg.ObserveServiceScopes {
		router.SetScopeEnforcement(false)
		log.Warn("service scopes are OBSERVED, not enforced: out-of-scope calls succeed and are logged")
	}
	p.router = router
	p.metrics = router.Metrics()

	p.Account = account.NewWithStore(log.With("domain", "account"), accountStore)
	if playerMinter != nil {
		p.Account.SetTokenMinter(playerMinter)
	}
	p.Team = team.NewWithStore(log.With("domain", "team"), teamStore)
	p.Economy = economy.NewWithStore(log.With("domain", "economy"), economyStore)
	p.Progress = progress.NewWithStore(log.With("domain", "progress"), progressStore)
	p.Rank = rank.NewWithStore(log.With("domain", "rank"), rankStore)
	p.Matchmaking = matchmaking.NewWithStore(log.With("domain", "matchmaking"), matchmakingStore, p.Rank.Rating)
	p.Session = session.NewWithStore(log.With("domain", "session"), sessionStore, alloc, &matchSink{
		log:      log.With("domain", "match_sink"),
		pool:     pool,
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

	// A scope naming a service this process does not serve is always a typo, and a typo
	// here makes the credential silently unable to call the thing it was meant to. Check
	// it once every op is registered.
	known := map[string]bool{}
	for _, service := range api.Services(p.router.Ops()) {
		known[service] = true
	}
	if unknown := creds.UnknownServices(known); len(unknown) > 0 {
		return nil, fmt.Errorf("platform: service credentials name unknown services: %v (known: %v)",
			unknown, api.Services(p.router.Ops()))
	}

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
			if err := p.ReconcileOnce(ctx, now); err != nil {
				p.log.Error("reconcile sweep failed", "err", err)
			}
		}
	}
}

// pendingReleasesPerTick bounds one retry sweep. A backlog is worked through over
// several ticks instead of holding the ticker hostage on a slow provider.
const pendingReleasesPerTick = 25

// ReconcileOnce runs one sweep: fail sessions that sat too long, retry the releases no
// provider confirmed, then publish the backlog.
//
// Exported so a test - or an operator with a shell - can trigger one pass without
// waiting for the ticker.
func (p *Platform) ReconcileOnce(ctx context.Context, now time.Time) error {
	expired, err := p.Session.ExpireStale(ctx, now, p.sessionStaleAfter)
	switch {
	case errors.Is(err, session.ErrNoExpirer):
		// A store that cannot scan is a configuration fact, not a failure: still retry
		// the releases, which only needs the pending rows.
		p.log.Warn("session store cannot sweep stale sessions")
	case err != nil:
		return fmt.Errorf("expire stale sessions: %w", err)
	case len(expired) > 0:
		p.log.Warn("expired stale sessions", "count", len(expired))
	}

	// Servers whose release was never confirmed. Retried here rather than in the request
	// path: a provider that is slow to let go must not slow down a player's settlement.
	result, err := p.Session.RetryReleases(ctx, pendingReleasesPerTick)
	if err != nil {
		return fmt.Errorf("retry releases: %w", err)
	}
	if result.Cleared > 0 {
		p.log.Info("pending releases confirmed", "count", result.Cleared)
	}

	// The backlog is published as numbers, not sentences: an alert watches a gauge, and
	// "servers we cannot confirm stopped" is the kind of thing that must page before an
	// invoice does.
	stats, err := p.Session.ReleaseStats(ctx, now)
	if err != nil {
		return fmt.Errorf("release stats: %w", err)
	}
	p.metrics.AddCounter("release_confirmed", int64(result.Cleared))
	p.metrics.AddCounter("release_failed", int64(result.Failed))
	p.metrics.SetGauge("release_pending_sessions", float64(stats.Pending))
	p.metrics.SetGauge("release_max_attempts", float64(stats.MaxAttempts))
	p.metrics.SetGauge("release_oldest_pending_seconds", float64(stats.OldestSeconds))
	return nil
}

func (p *Platform) ready(context.Context) error { return nil }
