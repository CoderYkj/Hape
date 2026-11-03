#!/bin/sh
# db-archive.sh - move finished, confirmed-released sessions into the archive tables.
#
# Why: sessions and their children only ever grow (spec 0018 leftover). The live tables are
# what the reconciler and the dashboard scan, so they should hold "recent" only.
#
# What is archived: finished or failed sessions older than ARCHIVE_BEFORE that nothing can
# still need - release_pending is false, and either the provider confirmed the stop
# (released_at set) or there was never an instance (allocation_id null). Their players, battle
# report and per-player results go with them.
#
# A session with release_pending = true is NEVER touched: the reconciler is still retrying
# its release, and moving it would make the retry loop's query silently miss it. Failed
# sessions are included because a failed allocation is still history that accumulates; a
# failed one that never got an instance has nothing to release at all.
#
# Safety:
#   - ARCHIVE_BEFORE is required and has no default. "How old is old" is a business
#     decision, and a script that guesses it will one day move a month of billing data.
#   - Dry run by default; only ARCHIVE_CONFIRM=yes writes.
#   - One transaction; child tables first so foreign keys never block the parent.
#   - Idempotent: archived rows are gone from the source, so a re-run is a no-op.
#
#   ARCHIVE_BEFORE=2026-01-01 DATABASE_URL=postgres://... sh scripts/db-archive.sh
#   ARCHIVE_CONFIRM=yes ARCHIVE_BEFORE=2026-01-01 DATABASE_URL=postgres://... sh scripts/db-archive.sh
set -eu

: "${DATABASE_URL:?set DATABASE_URL to a postgres:// URL}"
: "${ARCHIVE_BEFORE:?set ARCHIVE_BEFORE to the cutoff (YYYY-MM-DD or RFC3339): sessions created before it are archived}"

# A typo here would archive everything, so the shape is checked before psql sees it.
if ! printf '%s' "$ARCHIVE_BEFORE" | grep -Eq '^[0-9]{4}-[0-9]{2}-[0-9]{2}([T ][0-9]{2}:[0-9]{2}(:[0-9]{2})?)?'; then
  echo "ARCHIVE_BEFORE must look like 2026-01-01 or 2026-01-01T00:00:00Z, got: $ARCHIVE_BEFORE" >&2
  exit 1
fi

dry_run=1
if [ "${ARCHIVE_CONFIRM:-}" = "yes" ]; then
  dry_run=0
fi

sql_batch() {
  cat <<'SQL'
BEGIN;
CREATE TEMP TABLE archive_batch ON COMMIT DROP AS
  SELECT id FROM sessions
   WHERE state IN ('finished', 'failed')
     AND release_pending = false
     AND created_at < :'before'
     AND (released_at IS NOT NULL OR allocation_id IS NULL);

SELECT
  (SELECT count(*) FROM archive_batch)                              AS sessions,
  (SELECT count(*) FROM session_players sp
     JOIN archive_batch b ON b.id = sp.session_id)                  AS session_players,
  (SELECT count(*) FROM match_results mr
     JOIN archive_batch b ON b.id = mr.session_id)                  AS match_results,
  (SELECT count(*) FROM match_player_results mpr
     JOIN match_results mr ON mr.result_id = mpr.result_id
     JOIN archive_batch b ON b.id = mr.session_id)                  AS match_player_results;
SQL
  if [ "$dry_run" = "0" ]; then
    cat <<'SQL'
INSERT INTO session_players_archive (session_id, player_id)
  SELECT sp.session_id, sp.player_id FROM session_players sp
    JOIN archive_batch b ON b.id = sp.session_id
  ON CONFLICT DO NOTHING;

INSERT INTO match_player_results_archive (result_id, player_id, won, score, kills, deaths, damage)
  SELECT mpr.result_id, mpr.player_id, mpr.won, mpr.score, mpr.kills, mpr.deaths, mpr.damage
    FROM match_player_results mpr
    JOIN match_results mr ON mr.result_id = mpr.result_id
    JOIN archive_batch b ON b.id = mr.session_id
  ON CONFLICT DO NOTHING;

INSERT INTO match_results_archive (result_id, session_id, duration_seconds, accepted_at)
  SELECT mr.result_id, mr.session_id, mr.duration_seconds, mr.accepted_at
    FROM match_results mr
    JOIN archive_batch b ON b.id = mr.session_id
  ON CONFLICT DO NOTHING;

INSERT INTO sessions_archive (id, match_id, state, queue, region, map, allocation_id, provider,
                              address, port, result_id, runtime_token, created_at, ready_at,
                              started_at, finished_at, release_pending, release_attempts,
                              allocated_at, released_at)
  SELECT s.id, s.match_id, s.state, s.queue, s.region, s.map, s.allocation_id, s.provider,
         s.address, s.port, s.result_id, s.runtime_token, s.created_at, s.ready_at,
         s.started_at, s.finished_at, s.release_pending, s.release_attempts,
         s.allocated_at, s.released_at
    FROM sessions s
    JOIN archive_batch b ON b.id = s.id
  ON CONFLICT DO NOTHING;

DELETE FROM match_player_results mpr
  USING match_results mr, archive_batch b
  WHERE mpr.result_id = mr.result_id AND mr.session_id = b.id;
DELETE FROM match_results mr
  USING archive_batch b
  WHERE mr.session_id = b.id;
DELETE FROM session_players sp
  USING archive_batch b
  WHERE sp.session_id = b.id;
DELETE FROM sessions s
  USING archive_batch b
  WHERE s.id = b.id;
SQL
  fi
  echo "COMMIT;"
}

if [ "$dry_run" = "1" ]; then
  echo "dry run: nothing will be written (set ARCHIVE_CONFIRM=yes to archive)"
fi

sql_batch | psql -v ON_ERROR_STOP=1 -v before="$ARCHIVE_BEFORE" "$DATABASE_URL"