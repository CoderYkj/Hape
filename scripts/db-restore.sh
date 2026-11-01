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

echo "restoring $file into the database behind DATABASE_URL"
pg_restore --dbname="$DATABASE_URL" --clean --if-exists --no-owner --no-privileges --single-transaction "$file"
echo "restore complete"