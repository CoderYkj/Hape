package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The environment a managed provider injects into a dedicated server. This is the
// contract between the platform and whatever process ends up serving the match:
// the reference server in this directory reads it, and a real UE5 server has to
// read the same names (docs/ue5-ds-integration.md, spec 0012).
const (
	envBaseURL      = "BACKEND_BASE_URL"
	envServiceToken = "BACKEND_SERVICE_TOKEN"
	envSessionID    = "BATTLE_SESSION_ID"
	envMatchID      = "MATCH_ID"
	envRuntimeToken = "RUNTIME_TOKEN"
	envMap          = "MAP"
	envPlayers      = "PLAYERS"
	envPort         = "DS_PORT"
	envDuration     = "MATCH_DURATION_SECONDS"
	envResultsDir   = "RESULTS_DIR"
	envAdvertise    = "DS_ADVERTISE_ADDRESS"
	envCrash        = "DS_CRASH_BEFORE_REPORT"
)

// Config is the fully validated runtime configuration.
type Config struct {
	BaseURL      string
	ServiceToken string
	SessionID    string
	MatchID      string
	RuntimeToken string
	Map          string
	Players      []string
	Port         int
	Duration     time.Duration
	ResultsDir   string
	Advertise    string
	// CrashBeforeReport makes the server exit right after persisting its result id
	// and before reporting. It exists so the crash/restart drill can prove that a
	// restarted server reuses the id and the match is settled exactly once.
	CrashBeforeReport bool
}

// LoadConfig reads the runtime contract from getenv.
//
// Every required variable is named in the error, because the most likely failure
// is a provider that did not inject one, and "which one" is the whole diagnostic.
func LoadConfig(getenv func(string) string) (Config, error) {
	get := func(key string) string { return strings.TrimSpace(getenv(key)) }

	cfg := Config{
		BaseURL:      strings.TrimRight(get(envBaseURL), "/"),
		ServiceToken: get(envServiceToken),
		SessionID:    get(envSessionID),
		MatchID:      get(envMatchID),
		RuntimeToken: get(envRuntimeToken),
		Map:          get(envMap),
		ResultsDir:   get(envResultsDir),
		Advertise:    get(envAdvertise),
		Port:         7777,
		Duration:     3 * time.Second,
	}

	var missing []string
	for _, required := range []struct{ key, value string }{
		{envBaseURL, cfg.BaseURL},
		{envServiceToken, cfg.ServiceToken},
		{envSessionID, cfg.SessionID},
		{envRuntimeToken, cfg.RuntimeToken},
		{envPlayers, get(envPlayers)},
	} {
		if required.value == "" {
			missing = append(missing, required.key)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required environment: %s", strings.Join(missing, ", "))
	}

	players := []string{}
	if err := json.Unmarshal([]byte(get(envPlayers)), &players); err != nil {
		return Config{}, fmt.Errorf("%s must be a JSON array of player ids: %w", envPlayers, err)
	}
	if len(players) < 2 {
		return Config{}, fmt.Errorf("%s must contain at least 2 players, got %d", envPlayers, len(players))
	}
	cfg.Players = players

	if raw := get(envPort); raw != "" {
		port, err := strconv.Atoi(raw)
		if err != nil || port < 1 || port > 65535 {
			return Config{}, fmt.Errorf("%s must be a port between 1 and 65535, got %q", envPort, raw)
		}
		cfg.Port = port
	}

	if raw := get(envDuration); raw != "" {
		seconds, err := strconv.Atoi(raw)
		if err != nil || seconds < 0 {
			return Config{}, fmt.Errorf("%s must be a whole number of seconds, got %q", envDuration, raw)
		}
		cfg.Duration = time.Duration(seconds) * time.Second
	}

	if cfg.ResultsDir == "" {
		cfg.ResultsDir = "Saved/Results"
	}
	if cfg.Advertise == "" {
		// The reference server runs on the same host as the backend in every drill,
		// so loopback is the honest default. A real deployment injects the address
		// players will actually dial.
		cfg.Advertise = "127.0.0.1"
	}
	cfg.CrashBeforeReport = get(envCrash) == "1" || strings.EqualFold(get(envCrash), "true")

	return cfg, nil
}

// Banner is the one startup line the whole drill exists for: it proves the
// provider actually injected the configuration this process is using.
//
// Secrets are described by presence and length, never by value. A token that
// reaches a log has leaked, and "prove it arrived" does not require printing it.
func (c Config) Banner() string {
	return fmt.Sprintf(
		"ds: session=%s match=%s map=%q players=%d duration=%s listen_port=%d "+
			"runtime_token=present(len=%d) service_token=present(len=%d) base=%s results=%s crash_before_report=%t",
		c.SessionID, c.MatchID, c.Map, len(c.Players), c.Duration, c.Port,
		len(c.RuntimeToken), len(c.ServiceToken), c.BaseURL, c.ResultsDir, c.CrashBeforeReport)
}
