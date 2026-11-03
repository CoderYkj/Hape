package httpx

import (
	"math"
	"net"
	"net/http"
	"sync"
	"time"

	"gamebackend/pkg/api"
)

// RateLimit is one token bucket: a steady refill and a burst allowance.
//
// A zero PerSecond disables the bucket entirely - that is the switch a deployment flips
// when it wants a class unlimited, and it must not be confused with "burst 0", which
// would refuse every call.
type RateLimit struct {
	PerSecond float64
	Burst     float64
}

// Enabled reports whether this class is limited at all.
func (l RateLimit) Enabled() bool { return l.PerSecond > 0 }

// RateLimits are the buckets by caller class. Classes exist because the abuse they
// defend against differs: one player looping an enqueue is not the same event as the
// Nakama bridge being called twice as often as it should be.
type RateLimits struct {
	Player  RateLimit
	Service RateLimit
	Public  RateLimit
}

// For returns the bucket for a class name.
func (l RateLimits) For(class string) RateLimit {
	switch class {
	case "service":
		return l.Service
	case "public":
		return l.Public
	default:
		return l.Player
	}
}

// Any reports whether at least one class is limited.
func (l RateLimits) Any() bool {
	return l.Player.Enabled() || l.Service.Enabled() || l.Public.Enabled()
}

type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is a per-key token bucket, in memory, per process.
//
// It is not a distributed limiter: two replicas each allow the full rate. That is a
// known ceiling (spec 0020, M4), documented rather than hidden - the alternative was a
// shared store on the request path, which costs a round trip on every call to defend
// against something single-digit rps abuse.
type Limiter struct {
	mu      sync.Mutex
	limits  RateLimits
	buckets map[string]*bucket
	now     func() time.Time
	// sweepAt bounds memory: keys idle for longer than idleTTL are dropped once the map
	// grows past sweepAt. An attacker rotating keys (one identity per request) would
	// otherwise grow this without limit.
	sweepAt int
	idleTTL time.Duration
}

const (
	defaultMaxBuckets = 4096
	defaultIdleTTL    = 10 * time.Minute
)

// NewLimiter builds a limiter; nil means "no limiting at all" so callers can set it
// unconditionally.
func NewLimiter(limits RateLimits) *Limiter {
	if !limits.Any() {
		return nil
	}
	return &Limiter{
		limits:  limits,
		buckets: map[string]*bucket{},
		now:     time.Now,
		sweepAt: defaultMaxBuckets,
		idleTTL: defaultIdleTTL,
	}
}

// NewLimiterForTest is NewLimiter with an injectable clock, so the bucket arithmetic can
// be pinned without sleeping. Tests live in httpx_test and need it exported.
func NewLimiterForTest(limits RateLimits, now func() time.Time) *Limiter {
	l := NewLimiter(limits)
	if l == nil {
		l = &Limiter{limits: limits, buckets: map[string]*bucket{}, sweepAt: defaultMaxBuckets, idleTTL: defaultIdleTTL}
	}
	l.now = now
	return l
}

// Allow consumes one token for key, refilling first. A key that has never been seen
// starts full, so a burst right after boot is not mistaken for abuse.
func (l *Limiter) Allow(key string, limit RateLimit) bool {
	if !limit.Enabled() {
		return true
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if len(l.buckets) >= l.sweepAt {
		l.sweepLocked(now)
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: limit.Burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(limit.Burst, b.tokens+elapsed*limit.PerSecond)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (l *Limiter) sweepLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.last) > l.idleTTL {
			delete(l.buckets, k)
		}
	}
	// If nothing aged out (a genuinely busy keyspace), drop half rather than let the map
	// grow: forgetting a live bucket only grants its remaining burst again, which is
	// milder than unbounded memory.
	if len(l.buckets) >= l.sweepAt {
		cut := 0
		for k := range l.buckets {
			delete(l.buckets, k)
			cut++
			if cut >= l.sweepAt/2 {
				break
			}
		}
	}
}

// rateClass names the bucket and the key one request belongs to.
//
// Players and service credentials are keyed by identity, not by IP: behind a proxy every
// caller shares an IP, and a NAT'd player would be limited by their neighbours. Public
// ops (none today) fall back to the socket address, and X-Forwarded-For is deliberately
// ignored - without a trusted-proxy configuration it is attacker-controlled input.
func rateClass(op api.Op, p Principal, rq *http.Request) (class, key string) {
	switch op.Auth {
	case api.AuthPlayer:
		return "player", p.PlayerID
	case api.AuthService:
		return "service", p.Service
	default:
		host, _, err := net.SplitHostPort(rq.RemoteAddr)
		if err != nil {
			host = rq.RemoteAddr
		}
		return "public", host
	}
}
