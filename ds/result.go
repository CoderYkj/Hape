package main

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ResultState is the on-disk record that makes settlement restart-safe: the server
// persists the result id before reporting, so a crash between "report applied" and
// "process exited" cannot turn one match into two settlements.
//
// This is docs/ue5-ds-integration.md step 3, made executable.
type ResultState struct {
	SessionID  string     `json:"session_id"`
	ResultID   string     `json:"result_id"`
	CreatedAt  time.Time  `json:"created_at"`
	ReportedAt *time.Time `json:"reported_at,omitempty"`
}

// LoadOrCreateResult returns the result state for this session, creating it on first
// run and reusing it afterwards.
func LoadOrCreateResult(dir, sessionID string, now time.Time) (ResultState, error) {
	path, err := resultPath(dir, sessionID)
	if err != nil {
		return ResultState{}, err
	}

	raw, err := os.ReadFile(path)
	if err == nil {
		var state ResultState
		if err := json.Unmarshal(raw, &state); err != nil {
			return ResultState{}, fmt.Errorf("%s is not valid result state: %w", path, err)
		}
		if state.ResultID == "" {
			return ResultState{}, fmt.Errorf("%s has no result_id", path)
		}
		return state, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return ResultState{}, err
	}

	id, err := newResultID(sessionID, now)
	if err != nil {
		return ResultState{}, err
	}
	state := ResultState{SessionID: sessionID, ResultID: id, CreatedAt: now}
	if err := writeResult(path, state); err != nil {
		return ResultState{}, err
	}
	return state, nil
}

// MarkReported flips the persisted record to "done", so a restarted server exits
// instead of reporting a second time.
func MarkReported(dir, sessionID string, now time.Time) error {
	path, err := resultPath(dir, sessionID)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var state ResultState
	if err := json.Unmarshal(raw, &state); err != nil {
		return err
	}
	at := now
	state.ReportedAt = &at
	return writeResult(path, state)
}

func resultPath(dir, sessionID string) (string, error) {
	// The session id becomes a file name, so a separator (or a path traversal) must
	// fail loudly rather than write outside the results directory.
	if sessionID == "" || strings.ContainsAny(sessionID, `/\`) || strings.Contains(sessionID, "..") {
		return "", fmt.Errorf("session id %q cannot be used as a result file name", sessionID)
	}
	return filepath.Join(dir, sessionID+".json"), nil
}

func writeResult(path string, state ResultState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	// Temp file + rename: a half-written result file would be read back as garbage on
	// the restart that needs it most.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// newResultID builds "<session>-<unix>-<4 random chars>": unique per match, but
// reproducible from disk after a crash.
func newResultID(sessionID string, now time.Time) (string, error) {
	var raw [3]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	suffix := base32.StdEncoding.EncodeToString(raw[:])[:4]
	return fmt.Sprintf("%s-%d-%s", sessionID, now.Unix(), suffix), nil
}
