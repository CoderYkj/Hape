#!/bin/sh
# db-archive-restore.sh - bring archived sessions back into the live tables.
#
# Why a script instead of a documented INSERT: restoring touches four tables with foreign
# keys, and doing it by hand is how a half-restored session ends up permanently inconsistent.
# This does it in one transaction, in FK order, and refuses when the session is already live.
#
#   # dry run: shows how many rows would move
#   RESTORE_SESSION_IDS=s_abc123 DATABASE_URL=... sh scripts/db-archive-restore.sh
#
#   # restore
#   ARCHIVE_RESTORE_CONFIRM=yes RESTORE_SESSION_IDS=s_abc123,s_def456 DATABASE_URL=... \
#     sh scripts/db-archive-restore.sh
#
# Refuses when an id is already present in `sessions`: the archived copy would be deleted and
# the live row kept, silently dropping whatever the archive held.
set -eu

: "${DATABASE_URL:?set DATABASE_URL to a postgres:// URL}"
: "${RESTORE_SESSION_IDS:?set RESTORE_SESSION_IDS to comma-separated session ids (e.g. s_abc123)}"

# Ids are substituted into SQL below, so the charset is restricted here and nowhere else.
if ! printf '%s' "$RESTORE_SESSION_IDS" | grep -Eq '^[A-Za-z0-9_]+(,[A-Za-z0-9_]+)*$'; then
  echo "RESTORE_SESSION_IDS must be comma-separated ids (letters, digits, underscore): $RESTORE_SESSION_IDS" >&2
  exit 1
fi

dry_run=1
if [ "${ARCHIVE_RESTORE_CONFIRM:-}" = "yes" ]; then
  dry_run=0
fi

# "'s_a', 's_b'" - already validated, so quoting is the only transformation needed.
ids_sql="'$(printf '%s' "$RESTORE_SESSION_IDS" | sed "s/,/', '/g")'"

sql_restore() {
  cat <<'SQL'
BEGIN;

CREATE TEMP TABLE back_ids ON COMMIT DROP AS
  SELECT id FROM sessions_archive WHERE id IN (@IDS@);

DO $$
DECLARE live int;
BEGIN
  SELECT count(*) INTO live FROM sessions WHERE id IN (@IDS@);
  IF live > 0 THEN
    RAISE EXCEPTION 'refusing to restore: % of the requested ids are already live in sessions', live;
  END IF;
END $$;

SELECT count(*) AS sessions_to_restore FROM back_ids;
SQL
  if [ "$dry_run" = "0" ]; then
    cat <<'SQL'
INSERT INTO sessions (id, match_id, state, queue, region, map, allocation_id, provider, address,
                      port, result_id, runtime_token, created_at, ready_at, started_at,
                      finished_at, release_pending, release_attempts, allocated_at, released_at)
  SELECT s.id, s.match_id, s.state, s.queue, s.region, s.map, s.allocation_id, s.provider,
         s.address, s.port, s.result_id, s.runtime_token, s.created_at, s.ready_at,
         s.started_at, s.finished_at, s.release_pending, s.release_attempts,
         s.allocated_at, s.released_at
    FROM sessions_archive s
    JOIN back_ids b ON b.id = s.id
  ON CONFLICT DO NOTHING;

INSERT INTO session_players (session_id, player_id)
  SELECT sp.session_id, sp.player_id
    FROM session_players_archive sp
    JOIN back_ids b ON b.id = sp.session_id
  ON CONFLICT DO NOTHING;

INSERT INTO match_results (result_id, session_id, duration_seconds, accepted_at)
  SELECT mr.result_id, mr.session_id, mr.duration_seconds, mr.accepted_at
    FROM match_results_archive mr
    JOIN back_ids b ON b.id = mr.session_id
  ON CONFLICT DO NOTHING;

INSERT INTO match_player_results (result_id, player_id, won, score, kills, deaths, damage)
  SELECT mpr.result_id, mpr.player_id, mpr.won, mpr.score, mpr.kills, mpr.deaths, mpr.damage
    FROM match_player_results_archive mpr
    JOIN match_results_archive mr ON mr.result_id = mpr.result_id
    JOIN back_ids b ON b.id = mr.session_id
  ON CONFLICT DO NOTHING;

DELETE FROM match_player_results_archive mpr
  USING match_results_archive mr, back_ids b
  WHERE mpr.result_id = mr.result_id AND mr.session_id = b.id;
DELETE FROM match_results_archive mr
  USING back_ids b
  WHERE mr.session_id = b.id;
DELETE FROM session_players_archive sp
  USING back_ids b
  WHERE sp.session_id = b.id;
DELETE FROM sessions_archive s
  USING back_ids b
  WHERE s.id = b.id;
SQL
  fi
  echo "COMMIT;"
}

if [ "$dry_run" = "1" ]; then
  echo "dry run: nothing will be written (set ARCHIVE_RESTORE_CONFIRM=yes to restore)"
fi

sql_restore | sed "s/@IDS@/$ids_sql/g" |
  psql -v ON_ERROR_STOP=1 "$DATABASE_URL"