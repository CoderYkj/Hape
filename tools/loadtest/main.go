// Command loadtest drives N complete matches through a running backend and reports throughput
// plus per-phase latency percentiles.
//
// It exists to answer one question with a number instead of a guess: how many matches a single
// backend process can settle, and where the time goes. The setting is deliberately modest
// (loopback, dev auth, local allocator, same host), so read the output as a *baseline for this
// machine*, not as a production capacity claim - but a measured baseline beats an adjective
// (see docs/roadmap.md, M5).
//
//	go run ./tools/loadtest -matches 200 -concurrency 20
//	go run ./tools/loadtest -matches 100 -concurrency 10 -json
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// phases are the pieces of one match, in the order they happen. Keeping them separate is the
// point: "matches per second" without "which step is slow" is not actionable.
var phases = []string{"login", "ticket", "pop", "allocate", "ready", "report", "total"}

type client struct {
	base string
	http *http.Client
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "loadtest: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	base := flag.String("base", "http://127.0.0.1:8080", "backend base URL")
	matches := flag.Int("matches", 100, "how many complete matches to drive")
	concurrency := flag.Int("concurrency", 10, "how many matches in flight at once")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request timeout")
	asJSON := flag.Bool("json", false, "print JSON instead of a table")
	flag.Parse()

	if *matches <= 0 || *concurrency <= 0 {
		return fmt.Errorf("-matches and -concurrency must be positive")
	}

	c := &client{base: *base, http: &http.Client{Timeout: *timeout}}

	width := *concurrency
	if width > *matches {
		width = *matches
	}
	var (
		next     atomic.Int64
		failures atomic.Int64
		mu       sync.Mutex
		samples  = make([][]time.Duration, len(phases))
		started  = time.Now()
	)

	var wg sync.WaitGroup
	for w := 0; w < width; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				idx := int(next.Add(1)) - 1
				if idx >= *matches {
					return
				}
				durations, err := c.match(idx)
				if err != nil {
					failures.Add(1)
					fmt.Fprintf(os.Stderr, "match %d failed: %v\n", idx, err)
					continue
				}
				mu.Lock()
				for i, d := range durations {
					samples[i] = append(samples[i], d)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(started)

	ok := *matches - int(failures.Load())
	if *asJSON {
		return printJSON(*base, *matches, ok, int(failures.Load()), elapsed, samples)
	}
	printTable(*base, *matches, ok, int(failures.Load()), elapsed, samples)
	return nil
}

// match drives one whole match and returns the duration of each phase.
func (c *client) match(idx int) ([]time.Duration, error) {
	total := time.Now()
	out := make([]time.Duration, len(phases))

	// A queue per match keeps the matchmaker from pairing one loadtest match with another.
	queue := "loadtest_" + strconv.Itoa(idx)
	stamp := strconv.FormatInt(total.UnixNano(), 36)

	step := time.Now()
	players := make([]string, 2)
	tokens := make([]string, 2)
	for i := range players {
		var body struct {
			Player struct {
				ID string `json:"id"`
			} `json:"player"`
			SessionToken string `json:"session_token"`
		}
		if err := c.post("/v1/account/sessions", map[string]any{
			"external_id": fmt.Sprintf("loadtest-%d-%d-%s", idx, i, stamp),
		}, "dev-service", "", &body); err != nil {
			return nil, fmt.Errorf("login: %w", err)
		}
		players[i] = body.Player.ID
		tokens[i] = body.SessionToken
	}
	out[0] = time.Since(step)

	step = time.Now()
	for i := range players {
		if err := c.post("/v1/matchmaking/tickets", map[string]any{"queue": queue}, tokens[i], "", nil); err != nil {
			return nil, fmt.Errorf("ticket: %w", err)
		}
	}
	out[1] = time.Since(step)

	step = time.Now()
	var pop struct {
		Matched bool   `json:"matched"`
		MatchID string `json:"match_id"`
	}
	if err := c.post("/v1/matchmaking/queues/"+queue+"/pop", map[string]any{"count": 2}, "dev-service", "", &pop); err != nil {
		return nil, fmt.Errorf("pop: %w", err)
	}
	if !pop.Matched || pop.MatchID == "" {
		return nil, fmt.Errorf("pop returned no match")
	}
	out[2] = time.Since(step)

	step = time.Now()
	var session struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
		RuntimeToken string `json:"runtime_token"`
	}
	if err := c.post("/v1/session/sessions", map[string]any{
		"match_id": pop.MatchID,
		"players":  players,
		"queue":    queue,
		"map":      "arena_01",
	}, "dev-service", "", &session); err != nil {
		return nil, fmt.Errorf("create session: %w", err)
	}
	out[3] = time.Since(step)

	step = time.Now()
	if err := c.post("/v1/session/sessions/"+session.Session.ID+"/ready",
		map[string]any{"port": 7777}, "dev-service", session.RuntimeToken, nil); err != nil {
		return nil, fmt.Errorf("ready: %w", err)
	}
	out[4] = time.Since(step)

	step = time.Now()
	if err := c.post("/v1/session/sessions/"+session.Session.ID+"/results", map[string]any{
		"result_id":        "load-" + pop.MatchID,
		"duration_seconds": 60,
		"players": []map[string]any{
			{"player_id": players[0], "won": true, "score": 500, "kills": 5, "deaths": 2, "damage": 900},
			{"player_id": players[1], "won": false, "score": 200, "kills": 2, "deaths": 5, "damage": 400},
		},
	}, "dev-service", session.RuntimeToken, nil); err != nil {
		return nil, fmt.Errorf("report: %w", err)
	}
	out[5] = time.Since(step)
	out[6] = time.Since(total)
	return out, nil
}

func (c *client) post(path string, payload any, token, sessionToken string, into any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if sessionToken != "" {
		req.Header.Set("X-Session-Token", sessionToken)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, body)
	}
	if into != nil {
		return json.Unmarshal(body, into)
	}
	return nil
}

type summary struct {
	Base      string          `json:"base"`
	Matches   int             `json:"matches"`
	Succeeded int             `json:"succeeded"`
	Failed    int             `json:"failed"`
	ElapsedMS int64           `json:"elapsed_ms"`
	PerSecond float64         `json:"matches_per_second"`
	PerMinute float64         `json:"matches_per_minute"`
	Phases    map[string]stat `json:"phases"`
}

type stat struct {
	P50MS float64 `json:"p50_ms"`
	P95MS float64 `json:"p95_ms"`
	P99MS float64 `json:"p99_ms"`
	MaxMS float64 `json:"max_ms"`
}

func summarize(matches, ok, failed int, elapsed time.Duration, samples [][]time.Duration) summary {
	s := summary{
		Matches:   matches,
		Succeeded: ok,
		Failed:    failed,
		ElapsedMS: elapsed.Milliseconds(),
		Phases:    map[string]stat{},
	}
	if elapsed > 0 {
		s.PerSecond = float64(ok) / elapsed.Seconds()
		s.PerMinute = s.PerSecond * 60
	}
	for i, name := range phases {
		s.Phases[name] = percentile(samples[i])
	}
	return s
}

func percentile(values []time.Duration) stat {
	if len(values) == 0 {
		return stat{}
	}
	sorted := make([]time.Duration, len(values))
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	pick := func(p float64) float64 {
		idx := int(float64(len(sorted)-1) * p)
		return float64(sorted[idx].Microseconds()) / 1000
	}
	return stat{
		P50MS: pick(0.50),
		P95MS: pick(0.95),
		P99MS: pick(0.99),
		MaxMS: float64(sorted[len(sorted)-1].Microseconds()) / 1000,
	}
}

func printTable(base string, matches, ok, failed int, elapsed time.Duration, samples [][]time.Duration) {
	s := summarize(matches, ok, failed, elapsed, samples)
	fmt.Printf("target %s\n", base)
	fmt.Printf("%d matches, %d ok, %d failed in %s\n", s.Matches, s.Succeeded, s.Failed, elapsed.Round(time.Millisecond))
	fmt.Printf("throughput: %.1f matches/s (%.0f/min)\n\n", s.PerSecond, s.PerMinute)
	fmt.Printf("%-10s %9s %9s %9s %9s\n", "phase", "p50 ms", "p95 ms", "p99 ms", "max ms")
	for _, name := range phases {
		p := s.Phases[name]
		fmt.Printf("%-10s %9.2f %9.2f %9.2f %9.2f\n", name, p.P50MS, p.P95MS, p.P99MS, p.MaxMS)
	}
}

func printJSON(base string, matches, ok, failed int, elapsed time.Duration, samples [][]time.Duration) error {
	s := summarize(matches, ok, failed, elapsed, samples)
	s.Base = base
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return nil
}
