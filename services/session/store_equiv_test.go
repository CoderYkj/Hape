package session_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"gamebackend/pkg/apierr"
	"gamebackend/pkg/pg"
	"gamebackend/services/session"
)

// TestStoresAgree pins the session store's contract: one session per match, the
// state machine holds through Update, and the stale sweep only touches
// allocating/ready.
func TestStoresAgree(t *testing.T) {
	dsn := os.Getenv("BACKEND_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set BACKEND_TEST_DATABASE_URL to run the PostgreSQL/memory equality test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pg.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	defer pool.Close()

	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	matchID := "m_equiv_" + suffix
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM match_player_results WHERE result_id = $1`, "res_"+suffix)
		_, _ = pool.Exec(context.Background(), `DELETE FROM match_results WHERE result_id = $1`, "res_"+suffix)
		_, _ = pool.Exec(context.Background(), `DELETE FROM sessions WHERE match_id = $1`, matchID)
	})

	for _, tc := range []struct {
		name  string
		store session.Store
	}{
		{"memory", session.NewMemory()},
		{"postgres", session.NewPG(pool)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			created, err := tc.store.Create(ctx, session.Session{
				ID:        "s_" + tc.name + "_" + suffix,
				MatchID:   matchID,
				State:     session.StateAllocating,
				Players:   []string{"p_1", "p_2"},
				Queue:     "ranked_1v1",
				CreatedAt: time.Now().UTC(),
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			if created.State != session.StateAllocating {
				t.Fatalf("state = %q, want allocating", created.State)
			}

			// One session per match: the second create for the same match is refused.
			if _, err := tc.store.Create(ctx, session.Session{
				ID:      "s_dup_" + tc.name + "_" + suffix,
				MatchID: matchID,
				State:   session.StateAllocating,
				Players: []string{"p_1", "p_2"},
			}); apierr.As(err).Code != apierr.CodeConflict {
				t.Fatalf("duplicate match: code = %q, want conflict", apierr.As(err).Code)
			}

			// Illegal transition: allocating cannot jump straight to finished.
			if _, err := tc.store.Update(ctx, created.ID, func(sp *session.Session) error {
				if !session.CanTransition(sp.State, session.StateFinished) {
					return session.ErrInvalidTransition.WithDetail("from", sp.State)
				}
				sp.State = session.StateFinished
				return nil
			}); apierr.As(err).Code != apierr.CodeFailedPrecondition {
				t.Fatalf("illegal transition: code = %q, want failed_precondition", apierr.As(err).Code)
			}

			// Legal path: allocating -> ready -> finished with a result id.
			readied, err := tc.store.Update(ctx, created.ID, func(sp *session.Session) error {
				sp.State = session.StateReady
				sp.AllocationID = "alloc-1"
				sp.Provider = "local"
				sp.Address = "10.0.0.7"
				sp.Port = 7777
				now := time.Now().UTC()
				sp.ReadyAt = &now
				return nil
			})
			if err != nil {
				t.Fatalf("ready: %v", err)
			}
			if readied.State != session.StateReady || readied.AllocationID != "alloc-1" || readied.Port != 7777 {
				t.Fatalf("unexpected ready session: %+v", readied)
			}

			finished, err := tc.store.Update(ctx, created.ID, func(sp *session.Session) error {
				sp.State = session.StateFinished
				sp.ResultID = "res_" + suffix
				now := time.Now().UTC()
				sp.FinishedAt = &now
				return nil
			})
			if err != nil {
				t.Fatalf("finish: %v", err)
			}
			if finished.State != session.StateFinished || finished.ResultID != "res_"+suffix {
				t.Fatalf("unexpected finished session: %+v", finished)
			}
			// Read it back: Update's return value is not proof that it committed.
			persisted, err := tc.store.Get(ctx, created.ID)
			if err != nil {
				t.Fatalf("get after finish: %v", err)
			}
			if persisted.State != session.StateFinished || persisted.ResultID != "res_"+suffix {
				t.Fatalf("finish did not persist: %+v", persisted)
			}

			if _, err := tc.store.Get(ctx, "s_missing"); apierr.As(err).Code != apierr.CodeNotFound {
				t.Fatalf("unknown session: code = %q, want not_found", apierr.As(err).Code)
			}

			// The authoritative report must be on file, and a replay must not
			// duplicate it: that is the audit trail behind the payouts.
			recorder, ok := tc.store.(session.ResultRecorder)
			if !ok {
				t.Fatalf("%s store does not implement ResultRecorder", tc.name)
			}
			report := session.FinishedMatch{
				SessionID:       created.ID,
				MatchID:         matchID,
				ResultID:        "res_" + suffix,
				DurationSeconds: 300,
				Players: []session.PlayerResult{
					{PlayerID: "p_1", Won: true, Score: 900, Kills: 9, Deaths: 2, Damage: 1800},
					{PlayerID: "p_2", Won: false, Score: 400, Kills: 2, Deaths: 9, Damage: 700},
				},
				FinishedAt: time.Now().UTC(),
			}
			if err := recorder.RecordResult(ctx, report); err != nil {
				t.Fatalf("record result: %v", err)
			}
			if err := recorder.RecordResult(ctx, report); err != nil {
				t.Fatalf("record result replay: %v", err)
			}
			if tc.name == "postgres" {
				var headers, rows int
				if err := pool.QueryRow(context.Background(),
					`SELECT (SELECT count(*) FROM match_results WHERE result_id = $1),
					        (SELECT count(*) FROM match_player_results WHERE result_id = $1)`,
					"res_"+suffix).Scan(&headers, &rows); err != nil {
					t.Fatalf("count recorded rows: %v", err)
				}
				if headers != 1 || rows != 2 {
					t.Fatalf("recorded %d headers / %d player rows, want 1 / 2", headers, rows)
				}
			}

			// A finished session is not stale, however old it looks.
			expirer, ok := tc.store.(session.Expirer)
			if !ok {
				t.Fatalf("%s store does not implement Expirer", tc.name)
			}
			if expired, err := expirer.ExpireStale(ctx, time.Now().Add(24*time.Hour)); err != nil || len(expired) != 0 {
				t.Fatalf("finished session was swept: %+v (err %v)", expired, err)
			}

			// An abandoned allocating session is swept into failed.
			abandoned, err := tc.store.Create(ctx, session.Session{
				ID:        "s_stale_" + tc.name + "_" + suffix,
				MatchID:   matchID + "_stale",
				State:     session.StateAllocating,
				Players:   []string{"p_3", "p_4"},
				CreatedAt: time.Now().Add(-time.Hour).UTC(),
			})
			if err != nil {
				t.Fatalf("create abandoned: %v", err)
			}
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(), `DELETE FROM sessions WHERE match_id = $1`, matchID+"_stale")
			})

			expired, err := expirer.ExpireStale(ctx, time.Now())
			if err != nil {
				t.Fatalf("expire: %v", err)
			}
			if len(expired) != 1 || expired[0].ID != abandoned.ID {
				t.Fatalf("swept %+v, want only the abandoned session", expired)
			}
			after, err := tc.store.Get(ctx, abandoned.ID)
			if err != nil {
				t.Fatalf("get after sweep: %v", err)
			}
			if after.State != session.StateFailed {
				t.Fatalf("state after sweep = %q, want failed", after.State)
			}
		})
	}
}
