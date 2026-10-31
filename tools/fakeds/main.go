// Command fakeds simulates one UE5 dedicated server for the whole match loop.
//
// It is the offline stand-in for a real game server: it waits for a session,
// reports ready, plays for a moment, then posts the authoritative battle
// report. Use it to exercise the backend without building UE5.
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

func main() {
	base := flag.String("base", "http://127.0.0.1:8080", "backend base URL")
	queue := flag.String("queue", "ranked_1v1", "matchmaking queue")
	flag.Parse()

	if err := run(*base, *queue); err != nil {
		fmt.Fprintf(os.Stderr, "fakeds: %v\n", err)
		os.Exit(1)
	}
}

func run(base, queue string) error {
	service := client{base: base, token: "dev-service"}

	step("login two players")
	players := make([]string, 0, 2)
	tokens := make([]string, 0, 2)
	for i := 1; i <= 2; i++ {
		body, err := service.do(http.MethodPost, "/v1/account/sessions",
			map[string]any{"external_id": fmt.Sprintf("fakeds-player-%d", i)})
		if err != nil {
			return err
		}
		players = append(players, pickPath(body, "player.id"))
		tokens = append(tokens, pickPath(body, "session_token"))
	}
	fmt.Printf("  players: %v\n", players)

	step("enqueue both players")
	for i := range players {
		player := client{base: base, token: tokens[i]}
		if _, err := player.do(http.MethodPost, "/v1/matchmaking/tickets", map[string]any{"queue": queue}); err != nil {
			return err
		}
	}

	step("dequeue one match")
	body, err := service.do(http.MethodPost, "/v1/matchmaking/queues/"+queue+"/pop", map[string]any{"count": 2})
	if err != nil {
		return err
	}
	if matched, _ := body["matched"].(bool); !matched {
		return fmt.Errorf("queue did not produce a match; enqueue more players")
	}
	matchID := pickPath(body, "match_id")
	fmt.Printf("  match: %s\n", matchID)

	step("allocate a server")
	body, err = service.do(http.MethodPost, "/v1/session/sessions", map[string]any{
		"match_id": matchID,
		"players":  players,
		"queue":    queue,
		"map":      "arena_01",
	})
	if err != nil {
		return err
	}
	sessionID := pickPath(body, "session.id")
	dsToken := pickPath(body, "runtime_token")
	if dsToken == "" {
		return fmt.Errorf("session.create did not return a runtime token")
	}
	fmt.Printf("  session: %s state=%s provider=%s address=%s:%v\n", sessionID,
		pickPath(body, "session.state"), pickPath(body, "session.provider"),
		pickPath(body, "session.address"), body["session"].(map[string]any)["port"])

	step("report ready")
	if _, err := service.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/ready",
		map[string]any{"address": "127.0.0.1", "port": 7777}, "X-Session-Token", dsToken); err != nil {
		return err
	}

	step("play the match (1s)")
	time.Sleep(time.Second)

	step("post the authoritative battle report")
	resultID := fmt.Sprintf("fakeds-%d", time.Now().UnixNano())
	report := map[string]any{
		"result_id":        resultID,
		"duration_seconds": 1,
		"players": []map[string]any{
			{"player_id": players[0], "won": true, "score": 750, "kills": 7, "deaths": 3, "damage": 1400},
			{"player_id": players[1], "won": false, "score": 420, "kills": 3, "deaths": 7, "damage": 800},
		},
	}
	if _, err := service.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/results", report, "X-Session-Token", dsToken); err != nil {
		return err
	}

	step("replay the same report (must not pay twice)")
	replay, err := service.do(http.MethodPost, "/v1/session/sessions/"+sessionID+"/results", report, "X-Session-Token", dsToken)
	if err != nil {
		return err
	}
	if applied, _ := replay["applied"].(bool); applied {
		return fmt.Errorf("replayed report was applied twice")
	}

	step("read the fan-out results")
	winner := client{base: base, token: tokens[0]}
	wallet, err := winner.do(http.MethodGet, "/v1/economy/players/"+players[0]+"/wallet", nil)
	if err != nil {
		return err
	}
	progress, err := winner.do(http.MethodGet, "/v1/progress/players/"+players[0], nil)
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

	fmt.Println("\nOK: full match loop completed")
	return nil
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
