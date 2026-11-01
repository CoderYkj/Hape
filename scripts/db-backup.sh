#!/bin/sh
# db-backup.sh - logical backup of the backend database, in pg_dump's custom format.
#
# Custom format (-Fc) is what pg_restore wants: selective restore, parallel restore,
# and it compresses. The file is written to a temp name and renamed on success, so a
# crashed backup never looks like a complete one.
#
#   DATABASE_URL=postgres://... sh scripts/db-backup.sh /var/backups/gamebackend-$(date +%F).dump
set -eu

: "${DATABASE_URL:?set DATABASE_URL to a postgres:// URL}"
out="${1:?usage: db-backup.sh <output.dump>}"

if [ -e "$out" ]; then
  echo "refusing to overwrite $out" >&2
  exit 1
fi

mkdir -p "$(dirname "$out")"
tmp="$out.tmp.$$"
trap 'rm -f "$tmp"' EXIT INT TERM

pg_dump "$DATABASE_URL" --format=custom --no-owner --no-privileges --file="$tmp"
size="$(wc -c < "$tmp" | tr -d ' ')"
if [ "$size" -lt 512 ]; then
  echo "backup is only $size bytes - treating it as a failure" >&2
  exit 1
fi
mv "$tmp" "$out"
echo "backup written: $out ($size bytes)"