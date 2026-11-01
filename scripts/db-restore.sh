#!/bin/sh
# db-restore.sh - restore a pg_dump custom-format backup over the target database.
#
# Destructive by design: it drops and recreates every object in the dump's schema.
# It refuses to run unless RESTORE_CONFIRM=yes, because "restore" is also the name of
# the command somebody runs at 3am on the wrong database.
#
# Prefer restoring into a FRESH database and pointing the service at it, rather than
# overwriting a live one: pg_restore --clean drops objects in TOC order, which cannot
# satisfy foreign keys, so an in-place restore of a schema with relations fails (and
# "half restored" is worse than "not restored"). CI drills it that way.
#
#   RESTORE_CONFIRM=yes DATABASE_URL=postgres://... sh scripts/db-restore.sh backup.dump
set -eu

: "${DATABASE_URL:?set DATABASE_URL to a postgres:// URL}"
file="${1:?usage: db-restore.sh <backup.dump>}"

if [ ! -f "$file" ]; then
  echo "no such backup: $file" >&2
  exit 1
fi
if [ "${RESTORE_CONFIRM:-}" != "yes" ]; then
  echo "refusing to restore without RESTORE_CONFIRM=yes (this drops existing objects)" >&2
  exit 1
fi

# The dump records who wrote it (`pg_restore -l` prints "; Dumped by pg_dump version
# 17.x"), and a dump from a newer major carries SET statements an older server
# rejects. Fail before touching the database rather than half way through.
# The header is `;     Dumped by pg_dump version: 17.2` - leading spaces and a colon,
# so the pattern tolerates both. Getting this wrong silently disables the guard.
dump_major="$(pg_restore -l "$file" 2>/dev/null \
  | sed -n 's/^;[[:space:]]*Dumped by pg_dump version:[[:space:]]*\([0-9][0-9]*\)\..*/\1/p' \
  | head -n 1)"
server_major="$(psql -tAc 'SHOW server_version_num' "$DATABASE_URL" | cut -c1-2 | sed 's/^0//')"
if [ -n "$dump_major" ] && [ -n "$server_major" ] && [ "$dump_major" -gt "$server_major" ]; then
  echo "this dump was written by pg_dump $dump_major but the target server is $server_major," >&2
  echo "so it cannot be restored here. Restore into a server of at least major $dump_major." >&2
  exit 1
fi

echo "restoring $file into the database behind DATABASE_URL"
pg_restore --dbname="$DATABASE_URL" --clean --if-exists --no-owner --no-privileges --single-transaction "$file"
echo "restore complete"