package httpx_test

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gamebackend/pkg/api"
	"gamebackend/pkg/httpx"
)

// TestTokenBucketRefillsAndBursts pins the arithmetic: a fresh key starts with a full
// burst, each call costs one token, and time refills at PerSecond (capped at Burst).
func TestTokenBucketRefillsAndBursts(t *testing.T) {
	now := time.Unix(1700000000, 0)
	l := httpx.NewLimiterForTest(httpx.RateLimits{
		Service: httpx.RateLimit{PerSecond: 2, Burst: 3},
	}, func() time.Time { return now })
	limit := httpx.RateLimits{Service: httpx.RateLimit{PerSecond: 2, Burst: 3}}.Service

	for i := 0; i < 3; i++ {
		if !l.Allow("service:worker", limit) {
			t.Fatalf("burst call %d was refused", i+1)
		}
	}
	if l.Allow("service:worker", limit) {
		t.Fatal("a fourth call in the same instant must be refused")
	}
	// Half a second at 2 rps is one token.
	now = now.Add(500 * time.Millisecond)
	if !l.Allow("service:worker", limit) {
		t.Fatal("a refilled token was not granted")
	}
	if l.Allow("service:worker", limit) {
		t.Fatal("only one token had refilled")
	}
	// A long pause refills up to the burst, not beyond it.
	now = now.Add(time.Hour)
	for i := 0; i < 3; i++ {
		if !l.Allow("service:worker", limit) {
			t.Fatalf("after a pause, call %d should be allowed", i+1)
		}
	}
	if l.Allow("service:worker", limit) {
		t.Fatal("refill must cap at the burst")
	}
}

// TestDisabledClassIsUnlimited: 0 is the off switch, and it must not mean "burst 0".
func TestDisabledClassIsUnlimited(t *testing.T) {
	l := httpx.NewLimiterForTest(httpx.RateLimits{
		Service: httpx.RateLimit{PerSecond: 0, Burst: 0},
	}, time.Now)
	limit := httpx.RateLimit{PerSecond: 0, Burst: 0}
	for i := 0; i < 100; i++ {
		if !l.Allow("service:worker", limit) {
			t.Fatal("a disabled class must never refuse")
		}
	}
}

// TestRouterReturns429WithRetryAfter is the wire contract for an over-limit caller: 429,
// a Retry-After, the class in the details, and a counter that makes it queryable.
func TestRouterReturns429WithRetryAfter(t *testing.T) {
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := httpx.NewRouter("test", log, httpx.NewDevAuth("dev-service"))
	r.Handle(api.Op{
		ID: "t.ping", Service: "t", Method: http.MethodPost, Path: "/ping",
		Summary: "s", Response: api.T(struct{}{}), Auth: api.AuthService,
	}, func(*http.Request) (any, error) { return map[string]string{"ok": "1"}, nil })
	r.SetLimiter(httpx.NewLimiterForTest(httpx.RateLimits{
		Service: httpx.RateLimit{PerSecond: 0.001, Burst: 1},
	}, time.Now))
	r.Metrics().AddCounter("rate_limited_service", 0)

	call := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/ping", nil)
		req.Header.Set("Authorization", "Bearer dev-service")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
	if rec := call(); rec.Code != http.StatusOK {
		t.Fatalf("first call = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	rec := call()
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second call = %d, want 429 (%s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got == "" {
		t.Fatal("a 429 without Retry-After leaves the client guessing")
	}
	if !strings.Contains(rec.Body.String(), "rate limit exceeded") {
		t.Fatalf("body = %s", rec.Body)
	}
	var body strings.Builder
	mrec := httptest.NewRecorder()
	r.Metrics().Handler().ServeHTTP(mrec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body.WriteString(mrec.Body.String())
	if !strings.Contains(body.String(), `backend_rate_limited_service_total{service="test"} 1`) {
		t.Fatalf("refusal was not counted:\n%s", body.String())
	}
}
