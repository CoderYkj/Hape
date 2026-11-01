// Command fakeds runs one full match against the backend.
//
// Three roles, because there are three things that can be wrong:
//
//	(default)         act as the dedicated server in this process. The fastest loop:
//	                  allocate, report ready, report results. No container.
//	-launch-ds exec   act as the *provider* only: allocate a session, launch the
//	                  reference DS (ds/) with the environment a provider would inject,
//	                  and check what it did. Proves the runtime contract end to end.
//	-launch-ds docker same, but the DS is a container, which is the thing that ships.
//
// With -crash-restart the DS is killed before it reports and then restarted: the
// match must still settle exactly once, from the same persisted result id.
//
//	go run ./cmd/platform          # terminal 1
//	go run ./tools/fakeds          # terminal 2
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

type client struct {
	base  string
	token string
}

func (c client) do(method, path string, body any, headers ...string) (map[string]any, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var decoded map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, fmt.Errorf("%s %s: invalid JSON: %s", method, path, raw)
		}
	}
	if resp.StatusCode >= 300 {
		return decoded, fmt.Errorf("%s %s: status %d body %s", method, path, resp.StatusCode, raw)
	}
	return decoded, nil
}

type options struct {
	base         string
	queue        string
	launchDS     string
	dsImage      string
	dsCommand    string
	matchSeconds int
	crashRestart bool
	// mode is whose role this process plays: "play" (make a match of its own, the
	// original behaviour) or "worker" (wait for a real queue to produce a match, so a
	// real client can be one of the players).
	mode string
}

func main() {
	var opts options
	flag.StringVar(&opts.base, "base", "http://127.0.0.1:8080", "backend base URL")
	flag.StringVar(&opts.queue, "queue", "ranked_1v1", "matchmaking queue")
	flag.StringVar(&opts.launchDS, "launch-ds", "none",
		"who serves the match: none (this process), exec (reference DS binary) or docker (reference DS image)")
	flag.StringVar(&opts.dsImage, "ds-image", "hape-ds:ci", "image used with -launch-ds=docker")
	flag.StringVar(&opts.dsCommand, "ds-command", "",
		"explicit command for -launch-ds=exec; empty builds ./ds and runs the binary, "+
			"which keeps the real exit codes (go run would rewrite them)")
	flag.IntVar(&opts.matchSeconds, "match-seconds", 1, "match duration handed to the dedicated server")
	flag.BoolVar(&opts.crashRestart, "crash-restart", false,
		"kill the dedicated server before it reports, restart it, and check the match settles once")
	flag.StringVar(&opts.mode, "mode", "play",
		"play (make this process's own match) or worker (wait for a real queue, so a real client can join)")
	flag.Parse()

	var err error
	switch opts.mode {
	case "play":
		switch opts.launchDS {
		case "none":
			err = runBuiltin(opts)
		case "exec", "docker":
			err = runVendor(opts)
		default:
			err = fmt.Errorf("unknown -launch-ds %q (want none, exec or docker)", opts.launchDS)
		}
	case "worker":
		if opts.launchDS == "none" {
			// The worker is the provider: without a server to launch it would allocate a
			// session nobody serves.
			err = fmt.Errorf("-mode worker needs -launch-ds exec|docker")
		} else {
			err = runWorker(opts)
		}
	default:
		err = fmt.Errorf("unknown -mode %q (want play or worker)", opts.mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "fakeds: %v\n", err)
		os.Exit(1)
	}
}

// fixture is one allocated session plus the identities needed to talk about it.
type fixture struct {
	service      client
	players      []string
	tokens       []string
	matchID      string
	sessionID    string
	runtimeToken string
	address      string
	port         int
}

func (f fixture) playerAt(i int) client {
	return client{base: f.service.base, token: f.tokens[i]}
}

// openSession does everything up to and including the allocation.
func openSession(base, queue string) (fixture, error) {
	f := fixture{service: client{base: base, token: "dev-service"}}

	step("login two players")
	for i := 1; i <= 2; i++ {
		body, err := f.service.do(http.MethodPost, "/v1/account/sessions",
			map[string]any{"external_id": fmt.Sprintf("fakeds-player-%d", i)})
		if err != nil {
			return fixture{}, err
		}
		f.players = append(f.players, pickPath(body, "player.id"))
		f.tokens = append(f.tokens, pickPath(body, "session_token"))
	}
	fmt.Printf("  players: %v\n", f.players)

	step("enqueue both players")
	for i := range f.players {
		if _, err := f.playerAt(i).do(http.MethodPost, "/v1/matchmaking/tickets",
			map[string]any{"queue": queue}); err != nil {
			return fixture{}, err
		}
	}

	step("dequeue one match")
	body, err := f.service.do(http.MethodPost, "/v1/matchmaking/queues/"+queue+"/pop", map[string]any{"count": 2})
	if err != nil {
		return fixture{}, err
	}
	if matched, _ := body["matched"].(bool); !matched {
		return fixture{}, fmt.Errorf("queue did not produce a match; enqueue more players")
	}
	f.matchID = pickPath(body, "match_id")
	fmt.Printf("  match: %s\n", f.matchID)

	step("allocate a server")
	body, err = f.service.do(http.MethodPost, "/v1/session/sessions", map[string]any{
		"match_id": f.matchID,
		"players":  f.players,
		"queue":    queue,
		"map":      "arena_01",
	})
	if err != nil {
		return fixture{}, err
	}
	f.sessionID = pickPath(body, "session.id")
	f.runtimeToken = pickPath(body, "runtime_token")
	if f.runtimeToken == "" {
		return fixture{}, fmt.Errorf("session.create did not return a runtime token")
	}
	session, _ := body["session"].(map[string]any)
	f.address = pickPath(body, "session.address")
	if port, ok := session["port"].(float64); ok {
		f.port = int(port)
	}
	fmt.Printf("  session: %s state=%s provider=%s address=%s:%d\n", f.sessionID,
		pickPath(body, "session.state"), pickPath(body, "session.provider"), f.address, f.port)
	return f, nil
}

// runBuiltin is the original role: this process is the dedicated server.
func runBuiltin(opts options) error {
	f, err := openSession(opts.base, opts.queue)
	if err != nil {
		return err
	}

	step("report ready")
	if _, err := f.service.do(http.MethodPost, "/v1/session/sessions/"+f.sessionID+"/ready",
		map[string]any{"address": "127.0.0.1", "port": 7777}, "X-Session-Token", f.runtimeToken); err != nil {
		return err
	}

	step("play the match (1s)")
	time.Sleep(time.Second)

	step("post the authoritative battle report")
	report := reportFor(f, fmt.Sprintf("fakeds-%d", time.Now().UnixNano()))
	if _, err := f.service.do(http.MethodPost, "/v1/session/sessions/"+f.sessionID+"/results",
		report, "X-Session-Token", f.runtimeToken); err != nil {
		return err
	}

	step("replay the same report (must not pay twice)")
	replay, err := f.service.do(http.MethodPost, "/v1/session/sessions/"+f.sessionID+"/results",
		report, "X-Session-Token", f.runtimeToken)
	if err != nil {
		return err
	}
	if applied, _ := replay["applied"].(bool); applied {
		return fmt.Errorf("replayed report was applied twice")
	}

	step("read the fan-out results")
	if err := printSettlement(f, opts.queue); err != nil {
		return err
	}

	fmt.Println("\nOK: full match loop completed")
	return nil
}

// printSettlement reads back what the fan-out wrote. It prints rather than asserts:
// applied=true from session.submit_result already means the three sinks committed in
// one transaction (ADR 0007), so these reads are for the human reading the log.
func printSettlement(f fixture, queue string) error {
	winner := f.playerAt(0)
	wallet, err := winner.do(http.MethodGet, "/v1/economy/players/"+f.players[0]+"/wallet", nil)
	if err != nil {
		return err
	}
	progress, err := winner.do(http.MethodGet, "/v1/progress/players/"+f.players[0], nil)
	if err != nil {
		return err
	}
	board, err := winner.do(http.MethodGet, "/v1/rank/leaderboards/"+queue, nil)
	if err != nil {
		return err
	}
	pretty("wallet", wallet)
	pretty("progress", progress)
	pretty("leaderboard", board)
	return nil
}

func reportFor(f fixture, resultID string) map[string]any {
	return map[string]any{
		"result_id":        resultID,
		"duration_seconds": 1,
		"players": []map[string]any{
			{"player_id": f.players[0], "won": true, "score": 750, "kills": 7, "deaths": 3, "damage": 1400},
			{"player_id": f.players[1], "won": false, "score": 420, "kills": 3, "deaths": 7, "damage": 800},
		},
	}
}

func step(name string) { fmt.Printf("\n[%s]\n", name) }

func pretty(label string, v any) {
	raw, _ := json.MarshalIndent(v, "", "  ")
	fmt.Printf("%s: %s\n", label, raw)
}

func pickPath(body map[string]any, path string) string {
	var current any = body
	for _, part := range splitPath(path) {
		node, ok := current.(map[string]any)
		if !ok {
			return ""
		}
		current = node[part]
	}
	s, _ := current.(string)
	return s
}

func splitPath(path string) []string {
	var out []string
	start := 0
	for i := 0; i < len(path); i++ {
		if path[i] == '.' {
			out = append(out, path[start:i])
			start = i + 1
		}
	}
	return append(out, path[start:])
}
