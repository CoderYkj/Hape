package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// dsRun is one launch of the reference dedicated server.
type dsRun struct {
	output string
	exit   int
}

// runVendor plays the managed provider: this process allocates the session, but the
// match is served by a separate process or container that knows only what it was
// given in the environment.
//
// That separation is the entire point of the drill. Everything else about the match
// loop is already covered by the built-in role; what was never covered is whether the
// per-session credential actually reaches the instance that has to use it.
func runVendor(opts options) error {
	f, err := openSession(opts.base, opts.queue)
	if err != nil {
		return err
	}
	return serveMatch(opts, f, true)
}

// runWorker is the provider side of a match it did not invent: it waits for the queue
// to produce one, allocates a server for the roster that is actually queued, and then
// behaves exactly like runVendor.
//
// This is what lets a real client be one of the players. In play mode the tool logs in
// its own two players, so a client can only ever watch; here the roster comes from the
// tickets, so the client that enqueued is the one that gets the server address.
func runWorker(opts options) error {
	service := client{base: opts.base, token: "dev-service"}
	f := fixture{service: service}

	step("wait for a queued match")
	deadline := time.Now().Add(90 * time.Second)
	for {
		body, err := service.do(http.MethodPost, "/v1/matchmaking/queues/"+opts.queue+"/pop",
			map[string]any{"count": 2})
		if err != nil {
			return err
		}
		if matched, _ := body["matched"].(bool); matched {
			f.matchID = pickPath(body, "match_id")
			f.players = playersOf(body)
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no match formed in 90s; is a client enqueued on queue %q?", opts.queue)
		}
		time.Sleep(time.Second)
	}
	fmt.Printf("  match: %s players: %v\n", f.matchID, f.players)
	if len(f.players) < 2 {
		return fmt.Errorf("pop returned %d players, need at least 2", len(f.players))
	}

	step("allocate a server")
	body, err := service.do(http.MethodPost, "/v1/session/sessions", map[string]any{
		"match_id": f.matchID,
		"players":  f.players,
		"queue":    opts.queue,
		"map":      "arena_01",
	})
	if err != nil {
		return err
	}
	f.sessionID = pickPath(body, "session.id")
	f.runtimeToken = pickPath(body, "runtime_token")
	if f.runtimeToken == "" {
		return fmt.Errorf("session.create did not return a runtime token")
	}
	session, _ := body["session"].(map[string]any)
	f.address = pickPath(body, "session.address")
	if port, ok := session["port"].(float64); ok {
		f.port = int(port)
	}
	fmt.Printf("  session: %s state=%s provider=%s address=%s:%d\n", f.sessionID,
		pickPath(body, "session.state"), pickPath(body, "session.provider"), f.address, f.port)
	fmt.Printf("SESSION_ID=%s\n", f.sessionID)
	fmt.Printf("MATCH_ID=%s\n", f.matchID)

	// The worker holds no player credentials, so it cannot read anyone's wallet.
	return serveMatch(opts, f, false)
}

// playersOf flattens the popped tickets into the roster the session needs, including
// parties: a squad ticket carries its members in `party`.
func playersOf(body map[string]any) []string {
	var out []string
	tickets, _ := body["tickets"].([]any)
	for _, item := range tickets {
		ticket, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := ticket["player_id"].(string); id != "" {
			out = append(out, id)
		}
		party, _ := ticket["party"].([]any)
		for _, member := range party {
			if id, _ := member.(string); id != "" {
				out = append(out, id)
			}
		}
	}
	return out
}

// serveMatch is everything after the roster is known: start the server, check what it
// did, and read the settlement back when this process holds player credentials.
func serveMatch(opts options, f fixture, havePlayerTokens bool) error {
	resultsDir, err := os.MkdirTemp("", "hape-ds-results-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(resultsDir) }()
	if opts.launchDS == "docker" {
		// The image runs as an unprivileged user, and a bind mount carries the host
		// permissions into the container, so the drill has to open the directory up.
		if err := os.Chmod(resultsDir, 0o777); err != nil {
			return err
		}
	}
	fmt.Printf("  results directory: %s\n", resultsDir)

	step("launch the dedicated server (" + opts.launchDS + ")")
	first, err := launchDS(opts, f, resultsDir, opts.crashRestart)
	if err != nil {
		return err
	}
	printIndented("ds", first.output)

	// The assertion this whole item exists for: the provider carried the token, and
	// the process that had to use it says it has it.
	banner := fmt.Sprintf("runtime_token=present(len=%d)", len(f.runtimeToken))
	if !strings.Contains(first.output, banner) {
		return fmt.Errorf("the dedicated server did not report %q, so RUNTIME_TOKEN did not reach it", banner)
	}
	if strings.Contains(first.output, f.runtimeToken) {
		return fmt.Errorf("the dedicated server printed the RUNTIME_TOKEN value; a token in a log has leaked")
	}

	resultID := findResultID(first.output)
	if resultID == "" {
		return fmt.Errorf("the dedicated server never named its result id")
	}

	if opts.crashRestart {
		if first.exit != 17 {
			return fmt.Errorf("the crash run exited %d, want 17", first.exit)
		}
		step("restart the dedicated server (the persisted result id must survive)")
		second, err := launchDS(opts, f, resultsDir, false)
		if err != nil {
			return err
		}
		printIndented("ds", second.output)
		if again := findResultID(second.output); again != resultID {
			return fmt.Errorf("result id changed across the restart (%s -> %s): one match would settle twice", resultID, again)
		}
		if !strings.Contains(second.output, "applied=true") {
			return fmt.Errorf("the restarted server did not settle the match")
		}
		if second.exit != 0 {
			return fmt.Errorf("the restarted server exited %d", second.exit)
		}
	} else {
		if first.exit != 0 {
			return fmt.Errorf("the dedicated server exited %d", first.exit)
		}
		if !strings.Contains(first.output, "applied=true") {
			return fmt.Errorf("the battle report was not applied")
		}
	}

	step("check the backend agrees")
	if havePlayerTokens {
		body, err := f.playerAt(0).do(http.MethodGet, "/v1/session/sessions/"+f.sessionID, nil)
		if err != nil {
			return err
		}
		if state := pickPath(body, "session.state"); state != "finished" {
			return fmt.Errorf("session state is %q, want finished", state)
		}
		fmt.Printf("  session %s: finished\n", f.sessionID)
	} else {
		// The worker holds no player credentials, and session.get is player-scoped on
		// purpose, so it cannot read the session back - and does not need to: the
		// backend only answers applied=true after it committed the finished state.
		// The client reads its own settlement with its own token instead.
		fmt.Println("  applied=true is the confirmation: the session was finished before the backend answered")
	}

	if havePlayerTokens {
		if err := printSettlement(f, opts.queue); err != nil {
			return err
		}
	} else {
		fmt.Println("  (no player credentials here: the client reads its own settlement)")
	}

	fmt.Printf("\nOK: %s settled session %s (result_id=%s, runtime token %d bytes)\n",
		opts.launchDS, f.sessionID, resultID, len(f.runtimeToken))
	return nil
}

// launchDS starts the reference server the way a managed provider would: a bare
// process with the runtime contract in its environment, and nothing else.
func launchDS(opts options, f fixture, resultsDir string, crash bool) (dsRun, error) {
	playersJSON, err := json.Marshal(f.players)
	if err != nil {
		return dsRun{}, err
	}
	env := map[string]string{
		"BACKEND_BASE_URL":       f.service.base,
		"BACKEND_SERVICE_TOKEN":  f.service.token,
		"BATTLE_SESSION_ID":      f.sessionID,
		"RUNTIME_TOKEN":          f.runtimeToken,
		"PLAYERS":                string(playersJSON),
		"MATCH_ID":               f.matchID,
		"MAP":                    "arena_01",
		"DS_PORT":                "7777",
		"MATCH_DURATION_SECONDS": strconv.Itoa(opts.matchSeconds),
		"DS_ADVERTISE_ADDRESS":   "127.0.0.1",
	}
	if crash {
		env["DS_CRASH_BEFORE_REPORT"] = "1"
	}

	var cmd *exec.Cmd
	switch opts.launchDS {
	case "exec":
		binary, args, err := dsBinary(opts)
		if err != nil {
			return dsRun{}, err
		}
		env["RESULTS_DIR"] = resultsDir
		cmd = exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), flattenEnv(env)...)
	case "docker":
		// --network host: the container has to reach the backend on the host's
		// loopback and own the udp port it reports. The results directory is a volume,
		// so a crashed server's result id survives into the restarted container.
		args := []string{"run", "--rm", "--network", "host",
			"-v", resultsDir + ":/data", "-e", "RESULTS_DIR=/data"}
		for _, key := range sortedKeys(env) {
			args = append(args, "-e", key+"="+env[key])
		}
		args = append(args, opts.dsImage)
		cmd = exec.Command("docker", args...)
	default:
		return dsRun{}, fmt.Errorf("unknown launch mode %q", opts.launchDS)
	}

	out, err := cmd.CombinedOutput()
	run := dsRun{output: string(out)}
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return dsRun{}, err
		}
		run.exit = exitErr.ExitCode()
	}
	return run, nil
}

var resultIDPattern = regexp.MustCompile(`result_id=([^\s]+)`)

func findResultID(output string) string {
	if m := resultIDPattern.FindStringSubmatch(output); len(m) == 2 {
		return m[1]
	}
	return ""
}

func printIndented(label, output string) {
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		fmt.Printf("  %s| %s\n", label, line)
	}
}

func flattenEnv(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, key := range sortedKeys(env) {
		out = append(out, key+"="+env[key])
	}
	return out
}

func sortedKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	start := dir
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above %s", start)
		}
		dir = parent
	}
}

// dsBinary resolves the command to run in exec mode.
//
// Empty -ds-command means "build ./ds once, then run the binary". That is not a
// convenience: `go run` swallows the child's exit status and reports its own 1, which
// would make the crash drill's exit code 17 indistinguishable from a build failure -
// and a managed provider does not run a toolchain either.
var dsBinaryOnce struct {
	path string
	err  error
}

func dsBinary(opts options) (string, []string, error) {
	if opts.dsCommand != "" {
		fields := strings.Fields(opts.dsCommand)
		return fields[0], fields[1:], nil
	}
	if dsBinaryOnce.path != "" || dsBinaryOnce.err != nil {
		return dsBinaryOnce.path, nil, dsBinaryOnce.err
	}

	root, err := repoRoot()
	if err != nil {
		dsBinaryOnce.err = err
		return "", nil, err
	}
	name := fmt.Sprintf("hape-ds-%d", os.Getpid())
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	out := filepath.Join(os.TempDir(), name)

	build := exec.Command("go", "build", "-o", out, "./ds")
	build.Dir = root
	build.Env = os.Environ()
	if combined, err := build.CombinedOutput(); err != nil {
		dsBinaryOnce.err = fmt.Errorf("building ./ds failed: %w: %s", err, combined)
		return "", nil, dsBinaryOnce.err
	}
	dsBinaryOnce.path = out
	return out, nil, nil
}
