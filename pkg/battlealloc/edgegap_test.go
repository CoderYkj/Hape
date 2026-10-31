package battlealloc_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"gamebackend/pkg/battlealloc"
)

// fakeEdgegap is a scripted Edgegap API: deploy -> status -> stop. It records calls
// so the tests can assert what the adapter actually did (including that it stops a
// deployment it is about to abandon).
type fakeEdgegap struct {
	mu       sync.Mutex
	calls    []string
	authSeen []string

	deployStatus  int
	deployMessage string
	deployBody    map[string]any
	// statusReplies are consumed in order; the last one repeats.
	statusReplies []map[string]any
	statusIndex   int
	statusCodes   []int
	stopCodes     []int
	stopIndex     int
}

func (f *fakeEdgegap) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/deploy":
			_ = json.NewDecoder(r.Body).Decode(&f.deployBody)
			code := f.deployStatus
			if code == 0 {
				code = http.StatusCreated
			}
			w.WriteHeader(code)
			if code < 300 {
				_ = json.NewEncoder(w).Encode(map[string]any{"request_id": "req-1"})
			} else if f.deployMessage != "" {
				_, _ = w.Write([]byte(f.deployMessage))
			} else {
				_, _ = w.Write([]byte(`{"message":"nope"}`))
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/status/"):
			// One counter for both scripts: the n-th status call uses the n-th code
			// (last one repeats) and the n-th reply (last one repeats).
			i := f.statusIndex
			f.statusIndex++
			code := http.StatusOK
			if len(f.statusCodes) > 0 {
				code = f.statusCodes[min(i, len(f.statusCodes)-1)]
			}
			w.WriteHeader(code)
			if code < 300 {
				reply := f.statusReplies[min(i, len(f.statusReplies)-1)]
				_ = json.NewEncoder(w).Encode(reply)
			}
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/stop/"):
			code := http.StatusOK
			if f.stopIndex < len(f.stopCodes) {
				code = f.stopCodes[f.stopIndex]
			}
			f.stopIndex++
			w.WriteHeader(code)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func (f *fakeEdgegap) recorded() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeEdgegap) auths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authSeen...)
}

func readyStatus() map[string]any {
	return map[string]any{
		"request_id":     "req-1",
		"current_status": "READY",
		"running":        true,
		"public_ip":      "203.0.113.7",
		"fqdn":           "req-1.edgegap.net",
		"ports": map[string]any{
			"gameport": map[string]any{"internal": 7777, "external": 31234, "protocol": "UDP", "name": "gameport"},
		},
	}
}

func newAdapter(t *testing.T, fake *fakeEdgegap, mutate func(*battlealloc.EdgegapConfig)) *battlealloc.Edgegap {
	t.Helper()
	server := httptest.NewServer(fake.handler())
	t.Cleanup(server.Close)

	cfg := battlealloc.EdgegapConfig{
		Token:        "test-token",
		AppName:      "Hape",
		Version:      "demo-app-lyra",
		BaseURL:      server.URL,
		PollInterval: time.Millisecond,
		ReadyTimeout: 2 * time.Second,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	adapter, err := battlealloc.NewEdgegap(cfg)
	if err != nil {
		t.Fatalf("NewEdgegap: %v", err)
	}
	return adapter
}

func TestEdgegapAllocatesAReadyDeployment(t *testing.T) {
	fake := &fakeEdgegap{statusReplies: []map[string]any{
		{"request_id": "req-1", "current_status": "DEPLOYING", "running": false},
		readyStatus(),
	}}
	adapter := newAdapter(t, fake, nil)

	allocation, err := adapter.Allocate(context.Background(), battlealloc.Request{
		SessionID: "s_1",
		MatchID:   "m_1",
		IPs:       []string{"198.51.100.9"},
	})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if allocation.ID != "req-1" || allocation.Provider != "edgegap" {
		t.Fatalf("unexpected allocation: %+v", allocation)
	}
	if allocation.Address != "203.0.113.7" || allocation.Port != 31234 {
		t.Fatalf("address/port = %s:%d, want 203.0.113.7:31234", allocation.Address, allocation.Port)
	}

	// The request body must carry the contract the DS relies on.
	if got := fake.deployBody["app_name"]; got != "Hape" {
		t.Fatalf("app_name = %v", got)
	}
	if got := fake.deployBody["version_name"]; got != "demo-app-lyra" {
		t.Fatalf("version_name = %v", got)
	}
	ips, ok := fake.deployBody["ip_list"].([]any)
	if !ok || len(ips) != 1 || ips[0] != "198.51.100.9" {
		t.Fatalf("ip_list = %v, want the player IPs (geo selection)", fake.deployBody["ip_list"])
	}
	envs, _ := fake.deployBody["env_vars"].([]any)
	seen := map[string]string{}
	for _, e := range envs {
		m := e.(map[string]any)
		seen[m["key"].(string)] = m["value"].(string)
	}
	if seen["SESSION_ID"] != "s_1" || seen["MATCH_ID"] != "m_1" {
		t.Fatalf("env vars = %v", seen)
	}

	// Edgegap wants "Token <token>", not Bearer.
	for _, auth := range fake.auths() {
		if auth != "Token test-token" {
			t.Fatalf("Authorization = %q, want \"Token test-token\"", auth)
		}
	}
}

func TestEdgegapFallsBackToFQDN(t *testing.T) {
	status := readyStatus()
	delete(status, "public_ip")
	fake := &fakeEdgegap{statusReplies: []map[string]any{status}}
	adapter := newAdapter(t, fake, nil)

	allocation, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if allocation.Address != "req-1.edgegap.net" {
		t.Fatalf("address = %q, want the FQDN", allocation.Address)
	}
}

func TestEdgegapPortSelection(t *testing.T) {
	status := readyStatus()
	status["ports"] = map[string]any{
		"query": map[string]any{"internal": 7778, "external": 40000, "protocol": "UDP", "name": "query"},
		"game":  map[string]any{"internal": 7777, "external": 31234, "protocol": "UDP", "name": "gameport"},
	}
	adapter := newAdapter(t, &fakeEdgegap{statusReplies: []map[string]any{status}}, nil)
	allocation, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"})
	if err != nil {
		t.Fatalf("Allocate: %v", err)
	}
	if allocation.Port != 31234 {
		t.Fatalf("port = %d, want the port named gameport (31234)", allocation.Port)
	}

	// No usable port at all is a permanent failure, not a retry.
	broken := readyStatus()
	broken["ports"] = map[string]any{}
	adapter = newAdapter(t, &fakeEdgegap{statusReplies: []map[string]any{broken}}, nil)
	if _, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"}); !errors.Is(err, battlealloc.ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
}

func TestEdgegapStopsADeploymentThatFails(t *testing.T) {
	failed := map[string]any{
		"request_id":     "req-1",
		"current_status": "ERROR",
		"error":          true,
		"error_detail":   "image pull backoff",
	}
	fake := &fakeEdgegap{statusReplies: []map[string]any{failed}}
	adapter := newAdapter(t, fake, nil)

	_, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"})
	if !errors.Is(err, battlealloc.ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
	if !strings.Contains(err.Error(), "image pull backoff") {
		t.Fatalf("error %q should carry the provider detail", err.Error())
	}
	if !containsCall(fake.recorded(), "DELETE /v1/stop/req-1") {
		t.Fatalf("a failed deployment must be stopped, calls: %v", fake.recorded())
	}
}

func TestEdgegapTimesOutAndStopsTheDeployment(t *testing.T) {
	fake := &fakeEdgegap{statusReplies: []map[string]any{
		{"request_id": "req-1", "current_status": "DEPLOYING", "running": false},
	}}
	adapter := newAdapter(t, fake, func(c *battlealloc.EdgegapConfig) {
		c.ReadyTimeout = 30 * time.Millisecond
		c.PollInterval = 5 * time.Millisecond
	})

	_, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"})
	if !errors.Is(err, battlealloc.ErrRetryable) {
		t.Fatalf("err = %v, want ErrRetryable on timeout", err)
	}
	if !containsCall(fake.recorded(), "DELETE /v1/stop/req-1") {
		t.Fatalf("a timed-out deployment must be stopped, calls: %v", fake.recorded())
	}
}

func TestEdgegapClassifiesDeployErrors(t *testing.T) {
	cases := map[string]struct {
		status int
		want   error
	}{
		"unauthorized": {http.StatusUnauthorized, battlealloc.ErrPermanent},
		"forbidden":    {http.StatusForbidden, battlealloc.ErrPermanent},
		"bad request":  {http.StatusBadRequest, battlealloc.ErrPermanent},
		"rate limited": {http.StatusTooManyRequests, battlealloc.ErrRetryable},
		"server error": {http.StatusInternalServerError, battlealloc.ErrRetryable},
		"bad gateway":  {http.StatusBadGateway, battlealloc.ErrRetryable},
	}
	for name, tc := range cases {
		fake := &fakeEdgegap{deployStatus: tc.status, statusReplies: []map[string]any{readyStatus()}}
		adapter := newAdapter(t, fake, nil)
		_, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"})
		if !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", name, err, tc.want)
		}
	}
}

func TestEdgegapRetriesATransientStatusRead(t *testing.T) {
	fake := &fakeEdgegap{
		statusReplies: []map[string]any{readyStatus()},
		statusCodes:   []int{http.StatusBadGateway, http.StatusOK},
	}
	adapter := newAdapter(t, fake, nil)

	allocation, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"})
	if err != nil {
		t.Fatalf("Allocate should survive one bad status read: %v", err)
	}
	if allocation.Port != 31234 {
		t.Fatalf("unexpected allocation: %+v", allocation)
	}
}

// TestEdgegapDistinguishesQuotaFromABadToken pins a distinction found by calling the
// real API: HTTP 403 covers both "your token is not allowed" (permanent) and "your
// plan allows one deployment and it is already running" (capacity, retryable).
func TestEdgegapDistinguishesQuotaFromABadToken(t *testing.T) {
	quota := &fakeEdgegap{
		deployStatus:  http.StatusForbidden,
		deployMessage: `{"message":"Your plan allows you to create only 1 active deployments, and it appears you've reached this limit."}`,
		statusReplies: []map[string]any{readyStatus()},
	}
	adapter := newAdapter(t, quota, nil)
	if _, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"}); !errors.Is(err, battlealloc.ErrRetryable) {
		t.Fatalf("quota refusal: err = %v, want ErrRetryable", err)
	}

	badToken := &fakeEdgegap{
		deployStatus:  http.StatusForbidden,
		deployMessage: `{"message":"invalid token"}`,
		statusReplies: []map[string]any{readyStatus()},
	}
	adapter = newAdapter(t, badToken, nil)
	if _, err := adapter.Allocate(context.Background(), battlealloc.Request{SessionID: "s_1"}); !errors.Is(err, battlealloc.ErrPermanent) {
		t.Fatalf("bad token: err = %v, want ErrPermanent", err)
	}
}

func TestEdgegapReleaseIsIdempotent(t *testing.T) {
	fake := &fakeEdgegap{stopCodes: []int{http.StatusNotFound}}
	adapter := newAdapter(t, fake, nil)
	if err := adapter.Release(context.Background(), "req-1"); err != nil {
		t.Fatalf("releasing an already-stopped deployment must succeed: %v", err)
	}

	fake = &fakeEdgegap{stopCodes: []int{http.StatusInternalServerError}}
	adapter = newAdapter(t, fake, nil)
	if err := adapter.Release(context.Background(), "req-1"); !errors.Is(err, battlealloc.ErrRetryable) {
		t.Fatalf("err = %v, want ErrRetryable", err)
	}

	fake = &fakeEdgegap{stopCodes: []int{http.StatusUnauthorized}}
	adapter = newAdapter(t, fake, nil)
	if err := adapter.Release(context.Background(), "req-1"); !errors.Is(err, battlealloc.ErrPermanent) {
		t.Fatalf("err = %v, want ErrPermanent", err)
	}
}

func TestEdgegapRequiresConfiguration(t *testing.T) {
	if _, err := battlealloc.NewEdgegap(battlealloc.EdgegapConfig{AppName: "x"}); err == nil {
		t.Fatal("a missing token must be refused")
	}
	if _, err := battlealloc.NewEdgegap(battlealloc.EdgegapConfig{Token: "t"}); err == nil {
		t.Fatal("a missing app name must be refused")
	}
}

func containsCall(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}
