// Command ds is the reference dedicated server for this backend.
//
// It does what a UE5 dedicated server has to do, and nothing else: read the
// provider-injected environment, listen, report ready, play, report the
// authoritative result exactly once. It exists so the match loop can be exercised
// end-to-end - in a container, with the real credentials, on a real socket -
// without building UE5. docs/ue5-ds-integration.md describes what a UE5 server must
// do to replace it; the wire contract is identical.
//
// The environment is the whole interface:
//
//	BACKEND_BASE_URL        backend base URL
//	BACKEND_SERVICE_TOKEN   shared service credential
//	BATTLE_SESSION_ID       session this instance serves
//	RUNTIME_TOKEN           per-session credential injected by the provider
//	PLAYERS                 JSON array of backend player ids
//	MATCH_ID, MAP           copied onto the session/report
//	DS_PORT                 udp port, default 7777
//	MATCH_DURATION_SECONDS  how long the match lasts, default 3
//	RESULTS_DIR             where the result id is persisted, default Saved/Results
//	DS_ADVERTISE_ADDRESS    address players dial, default 127.0.0.1
//	DS_CRASH_BEFORE_REPORT  "1" exits before reporting, for the restart drill
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

// exitCrashedBeforeReport is the drill's crash code: distinct from 1 so a script can
// tell "the deliberate crash happened" from "the server failed".
const exitCrashedBeforeReport = 17

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "ds: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	fmt.Println(cfg.Banner())

	state, err := LoadOrCreateResult(cfg.ResultsDir, cfg.SessionID, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("preparing the result id: %w", err)
	}
	if state.ReportedAt != nil {
		fmt.Printf("ds: session=%s already reported (result_id=%s at %s), exiting\n",
			cfg.SessionID, state.ResultID, state.ReportedAt.Format(time.RFC3339))
		return nil
	}

	// Bind before reporting: "ready" means ready for players, and the port the
	// backend hands out has to be a port this process actually owns.
	conn, err := net.ListenPacket("udp", fmt.Sprintf(":%d", cfg.Port))
	if err != nil {
		return fmt.Errorf("cannot listen on udp :%d: %w", cfg.Port, err)
	}
	defer func() { _ = conn.Close() }()
	port := conn.LocalAddr().(*net.UDPAddr).Port

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &Client{
		BaseURL:      cfg.BaseURL,
		ServiceToken: cfg.ServiceToken,
		RuntimeToken: cfg.RuntimeToken,
		Logf:         func(format string, args ...any) { fmt.Printf(format+"\n", args...) },
	}
	if err := client.ReportReady(ctx, cfg.SessionID, cfg.Advertise, port); err != nil {
		return fmt.Errorf("reporting ready: %w", err)
	}
	fmt.Printf("ds: ready session=%s address=%s port=%d\n", cfg.SessionID, cfg.Advertise, port)

	packets := play(ctx, conn, cfg.Duration)
	fmt.Printf("ds: match over duration=%s packets=%d result_id=%s\n", cfg.Duration, packets, state.ResultID)

	if cfg.CrashBeforeReport {
		fmt.Printf("ds: crash drill - exiting before reporting result_id=%s\n", state.ResultID)
		os.Exit(exitCrashedBeforeReport)
	}

	report := BuildReport(cfg, state.ResultID)
	fmt.Printf("ds: reporting result_id=%s\n", state.ResultID)
	applied, err := client.SubmitResult(ctx, cfg.SessionID, report)
	if err != nil {
		return fmt.Errorf("submitting the battle report: %w", err)
	}
	if err := MarkReported(cfg.ResultsDir, cfg.SessionID, time.Now().UTC()); err != nil {
		return fmt.Errorf("marking the result reported: %w", err)
	}
	fmt.Printf("ds: match complete applied=%t result_id=%s\n", applied, state.ResultID)
	return nil
}

// play drains whatever players send until the match clock runs out. Receiving
// packets is not required - the drill has no game client - but the socket has to be
// alive for the whole match, because that is what "ready" promised.
func play(ctx context.Context, conn net.PacketConn, d time.Duration) int {
	deadline := time.Now().Add(d)
	buf := make([]byte, 2048)
	count := 0
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 || ctx.Err() != nil {
			return count
		}
		_ = conn.SetReadDeadline(time.Now().Add(min(remaining, 200*time.Millisecond)))
		if _, _, err := conn.ReadFrom(buf); err == nil {
			count++
		}
	}
}

// BuildReport is the authoritative result. The reference server awards the match to
// the first player in the session; the numbers are derived from the roster, so the
// drill's assertions are stable across runs.
func BuildReport(cfg Config, resultID string) Report {
	players := make([]PlayerLine, 0, len(cfg.Players))
	for i, id := range cfg.Players {
		line := PlayerLine{PlayerID: id, Won: false, Score: 500, Kills: 5, Deaths: 3, Damage: 900}
		if i == 0 {
			line = PlayerLine{PlayerID: id, Won: true, Score: 750, Kills: 7, Deaths: 3, Damage: 1400}
		}
		players = append(players, line)
	}
	return Report{
		ResultID:        resultID,
		DurationSeconds: int64(cfg.Duration / time.Second),
		Players:         players,
	}
}
