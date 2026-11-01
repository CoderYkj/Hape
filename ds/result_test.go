package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResultIDIsReusedAfterRestart(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()

	first, err := LoadOrCreateResult(dir, "s_restart", now)
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	// A restarted process must not mint a new id: that is what turns one match into
	// two settlements.
	second, err := LoadOrCreateResult(dir, "s_restart", now.Add(time.Minute))
	if err != nil {
		t.Fatalf("second load: %v", err)
	}
	if first.ResultID != second.ResultID {
		t.Fatalf("result id changed across restarts: %s then %s", first.ResultID, second.ResultID)
	}
	if second.ReportedAt != nil {
		t.Fatal("a fresh result must not look reported")
	}
}

func TestMarkReportedSurvivesAReload(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateResult(dir, "s_mark", time.Now().UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := MarkReported(dir, "s_mark", time.Now().UTC()); err != nil {
		t.Fatalf("mark: %v", err)
	}
	state, err := LoadOrCreateResult(dir, "s_mark", time.Now().UTC())
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if state.ReportedAt == nil {
		t.Fatal("reported_at did not persist; a restarted server would report twice")
	}
}

func TestResultFileIsWrittenAtomically(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadOrCreateResult(dir, "s_atomic", time.Now().UTC()); err != nil {
		t.Fatalf("create: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the result file, found %d entries", len(entries))
	}
	if filepath.Ext(entries[0].Name()) != ".json" {
		t.Fatalf("unexpected file %q (a leftover .tmp would mean the rename never happened)", entries[0].Name())
	}
}

func TestResultPathRejectsSessionIDsThatEscapeTheDirectory(t *testing.T) {
	for _, id := range []string{"", "../escape", "a/b", `a\b`} {
		if _, err := LoadOrCreateResult(t.TempDir(), id, time.Now().UTC()); err == nil {
			t.Fatalf("session id %q should have been rejected", id)
		}
	}
}
