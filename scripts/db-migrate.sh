#!/bin/sh
# db-migrate.sh - apply db/migrations/*.sql in order, exactly once each.
#
# Written for the deploy step (and CI). Behaviour worth knowing:
#   - files are recorded in schema_migrations with their sha256, so re-running is a
#     no-op instead of an error;
#   - an already-applied file whose content changed is a hard failure: migrations are
#     append-only, and editing one silently is how a schema drifts from every copy of
#     the repository;
#   - each file runs in its own psql invocation with ON_ERROR_STOP, so a failure stops
#     the run instead of leaving half a migration behind.
#
#   DATABASE_URL=postgres://user:pass@host:5432/db sh scripts/db-migrate.sh
set -eu

: "${DATABASE_URL:?set DATABASE_URL to a postgres:// URL}"
MIGRATIONS_DIR="${MIGRATIONS_DIR:-db/migrations}"

if [ ! -d "$MIGRATIONS_DIR" ]; then
  echo "no migrations directory at $MIGRATIONS_DIR" >&2
  exit 1
fi

# Options before the connection string: PostgreSQL's getopt does not permute them
# past the first positional argument, so `psql "$url" -c ...` works on glibc and
# silently drops the query on Windows.
psql -v ON_ERROR_STOP=1 -q -c \
  'CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, checksum text NOT NULL, applied_at timestamptz NOT NULL DEFAULT now())' \
  "$DATABASE_URL"

# "CREATE TABLE IF NOT EXISTS" is happy with a table of the right name and the wrong
# shape, and then every later query fails with "column checksum does not exist". A
# bookkeeping table this script did not create is a real situation (a hand-built
# database, an older tool), so say what is wrong instead of failing three lines later.
# Column order is not a contract; having the three columns is.
missing="$(psql -tAc \
  "SELECT string_agg(req.name, ', ') FROM (VALUES ('version'), ('checksum'), ('applied_at')) AS req(name)
    WHERE req.name NOT IN (
      SELECT column_name FROM information_schema.columns
       WHERE table_schema = current_schema() AND table_name = 'schema_migrations')" "$DATABASE_URL")"
if [ -n "$missing" ]; then
  echo "schema_migrations is missing: $missing" >&2
  echo "this script needs (version, checksum, applied_at). If the table was built by hand," >&2
  echo "add the missing column and backfill its checksums (sha256 of each migration file)," >&2
  echo "or drop the table and re-apply every migration against an empty database." >&2
  exit 1
fi

applied=0
skipped=0

for file in "$MIGRATIONS_DIR"/*.sql; do
  [ -e "$file" ] || { echo "no .sql files in $MIGRATIONS_DIR" >&2; exit 1; }
  version="$(basename "$file")"
  checksum="$(sha256sum "$file" | cut -d' ' -f1)"

  recorded="$(psql -tAc \
    "SELECT checksum FROM schema_migrations WHERE version = '$version'" "$DATABASE_URL")"

  if [ -n "$recorded" ]; then
    if [ "$recorded" != "$checksum" ]; then
      echo "migration $version changed after being applied ($recorded -> $checksum)." >&2
      echo "Migrations are append-only: add a new file instead of editing an old one." >&2
      exit 1
    fi
    echo "skip   $version (already applied)"
    skipped=$((skipped + 1))
    continue
  fi

  echo "apply  $version"
  psql -v ON_ERROR_STOP=1 -q -f "$file" "$DATABASE_URL"
  psql -v ON_ERROR_STOP=1 -q -c \
    "INSERT INTO schema_migrations (version, checksum) VALUES ('$version', '$checksum')" \
    "$DATABASE_URL"
  applied=$((applied + 1))
done

echo "migrations: $applied applied, $skipped already present"