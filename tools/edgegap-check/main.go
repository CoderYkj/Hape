// Command edgegap-check deploys one dedicated server through Edgegap, stops it again, and
// waits for the provider to confirm the instance is gone.
//
// It is the smallest probe that the adapter, the token, the application and the port
// mapping all agree with reality - and the only way to see the provider's real
// current_status sequence, which is what spec 0015's release read-back assumes.
//
// It spends a few seconds of instance time, so it is a tool and not a test.
//
// It prints the provider's status sequence, which is the part no unit test can know: the
// first live run (2026-11-01) showed that `running` flips false while `current_status` is
// still `Status.TERMINATING`, which is why the adapter decides on `running`.
//
//	EDGEGAP_TOKEN=... EDGEGAP_APP=Hape EDGEGAP_VERSION=demo-app-lyra \
//	  go run ./tools/edgegap-check
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
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

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func run() error {
	token := flag.String("token", os.Getenv("EDGEGAP_TOKEN"), "Edgegap API token")
	app := flag.String("app", os.Getenv("EDGEGAP_APP"), "Edgegap application name")
	version := flag.String("version", os.Getenv("EDGEGAP_VERSION"), "Edgegap application version (empty = latest)")
	baseURL := flag.String("base-url", envOr("EDGEGAP_BASE_URL", "https://api.edgegap.com"), "Edgegap API base URL")
	timeout := flag.Duration("timeout", 4*time.Minute, "how long to wait for the instance to be ready")
	stopTimeout := flag.Duration("stop-timeout", 60*time.Second, "how long the adapter may wait for the provider to confirm the stop")
	confirmBy := flag.Duration("confirm-by", 90*time.Second, "give up on confirmation after this long")
	poll := flag.Duration("poll", 2*time.Second, "status sampling interval")
	lat := flag.Float64("lat", envFloat("EDGEGAP_LAT", 0), "location latitude (fallback when no player IPs)")
	lon := flag.Float64("lon", envFloat("EDGEGAP_LON", 0), "location longitude")
	ipHint := flag.String("ip", os.Getenv("EDGEGAP_IP"), "player IP to place the instance near (preferred over lat/lon)")
	keep := flag.Bool("keep", false, "leave the deployment running (it will be billed and auto-expire)")
	flag.Parse()

	if *token == "" || *app == "" {
		return errors.New("EDGEGAP_TOKEN and EDGEGAP_APP are required")
	}

	cfg := battlealloc.EdgegapConfig{
		Token:            *token,
		AppName:          *app,
		Version:          *version,
		BaseURL:          *baseURL,
		ReadyTimeout:     *timeout,
		PollInterval:     2 * time.Second,
		StopTimeout:      *stopTimeout,
		StopPollInterval: time.Second,
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
	req := battlealloc.Request{SessionID: sessionID, MatchID: "smoke-match", Queue: "smoke", RuntimeToken: "smoke-token"}
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

	// Start the sampler before the stop request so the first observation is the running
	// state: the sequence is the evidence, not just the final answer.
	sampler := newSampler(*baseURL, *token, allocation.ID, *poll)
	sampler.start()

	fmt.Printf("releasing %s ...\n", allocation.ID)
	releaseStart := time.Now()
	confirmCtx, cancelConfirm := context.WithTimeout(context.Background(), *confirmBy)
	defer cancelConfirm()
	releaseErr := allocator.Release(confirmCtx, allocation.ID)
	elapsed := time.Since(releaseStart)
	sampler.stop()
	sampler.wait()

	sequence := sampler.sequence()
	fmt.Printf("status sequence over %s: %s\n", elapsed.Round(time.Millisecond), strings.Join(sequence, " -> "))
	if releaseErr != nil {
		return fmt.Errorf("release %s NOT confirmed after %s (STOP IT MANUALLY in the Edgegap console): %w",
			allocation.ID, elapsed.Round(time.Second), releaseErr)
	}
	last := sampler.last()
	fmt.Printf("CONFIRMED released in %s: last observed status=%q running=%t\n",
		elapsed.Round(time.Second), last.CurrentStatus, last.Running)
	fmt.Println("nothing should be billing for this deployment")
	return nil
}

// observation is the subset of the status response the probe reports.
type observation struct {
	CurrentStatus string `json:"current_status"`
	Running       bool   `json:"running"`
}

// sampler polls the deployment status independently of the adapter.
//
// The adapter's read-back decides success; this exists to record *what the provider said
// on the way there*, which is the part no unit test can know.
type sampler struct {
	baseURL string
	token   string
	id      string
	poll    time.Duration
	client  *http.Client

	mu      sync.Mutex
	seq     []string
	lastObs observation
	stopped bool
	stopCh  chan struct{}
	exitCh  chan struct{}
}

func newSampler(baseURL, token, id string, poll time.Duration) *sampler {
	return &sampler{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		id:      id,
		poll:    poll,
		client:  &http.Client{Timeout: 10 * time.Second},
		stopCh:  make(chan struct{}),
		exitCh:  make(chan struct{}),
	}
}

func (s *sampler) start() {
	go func() {
		defer close(s.exitCh)
		for {
			s.sample()
			select {
			case <-s.stopCh:
				return
			case <-time.After(s.poll):
			}
		}
	}()
}

func (s *sampler) stop() {
	s.mu.Lock()
	if !s.stopped {
		s.stopped = true
		close(s.stopCh)
	}
	s.mu.Unlock()
}

func (s *sampler) wait() { <-s.exitCh }

func (s *sampler) sample() {
	req, err := http.NewRequest(http.MethodGet, s.baseURL+"/v1/status/"+s.id, nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Token "+s.token)
	resp, err := s.client.Do(req)
	if err != nil {
		s.record("status-unreadable", observation{})
		return
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusNotFound {
		s.record("gone(404)", observation{})
		return
	}
	var obs observation
	if err := json.Unmarshal(raw, &obs); err != nil {
		return
	}
	s.record(obs.CurrentStatus, obs)
}

func (s *sampler) record(status string, obs observation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.seq); n == 0 || s.seq[n-1] != status {
		s.seq = append(s.seq, status)
	}
	if status != "status-unreadable" && status != "gone(404)" {
		s.lastObs = obs
	}
}

func (s *sampler) sequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.seq...)
}

func (s *sampler) last() observation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastObs
}
