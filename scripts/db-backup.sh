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

# A dump is only as good as the client that wrote it. pg_dump 17 emits
# `SET transaction_timeout = 0`, which server 16 rejects, so a newer client silently
# produces a backup that cannot be restored into the very database it came from -
# and you find out during the incident. Refuse to write it instead.
client_major="$(pg_dump --version | sed -n 's/.* \([0-9][0-9]*\)\..*/\1/p')"
server_major="$(psql -tAc 'SHOW server_version_num' "$DATABASE_URL" | cut -c1-2 | sed 's/^0//')"
if [ -n "$client_major" ] && [ -n "$server_major" ] && [ "$client_major" -gt "$server_major" ]; then
  echo "pg_dump $client_major against a $server_major server: this dump would not restore here." >&2
  echo "use the matching client (with docker: postgres:$server_major) and try again." >&2
  exit 1
fi

pg_dump --format=custom --no-owner --no-privileges --file="$tmp" "$DATABASE_URL"
size="$(wc -c < "$tmp" | tr -d ' ')"
if [ "$size" -lt 512 ]; then
  echo "backup is only $size bytes - treating it as a failure" >&2
  exit 1
fi
mv "$tmp" "$out"
echo "backup written: $out ($size bytes)"