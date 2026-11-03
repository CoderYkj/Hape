#!/usr/bin/env bash
# verify — the single gate for this repo (Linux / macOS / CI).
#
#   ./scripts/verify.sh          # gofmt + vet + build + test + contract
#   RACE=1 ./scripts/verify.sh   # same, with the race detector
set -uo pipefail

cd "$(dirname "$0")/.."

# Keep Go caches out of the working tree.
cache_root="${TMPDIR:-/tmp}/gamebackend"
export GOCACHE="$cache_root/go-build"
export GOMODCACHE="$cache_root/go-mod"
export GOPATH="$cache_root/gopath"
export GOFLAGS="-buildvcs=false"
mkdir -p "$GOCACHE" "$GOMODCACHE" "$GOPATH"

fail=0
failed_steps=()

step() { printf '\n==> %s\n' "$1"; }
record() { fail=1; failed_steps+=("$1"); }

step "gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  printf '%s\n' "$unformatted"
  record "gofmt"
fi

step "go vet"
go vet ./... || record "go vet"

step "go build"
go build ./... || record "go build"

if [ "${RACE:-0}" = "1" ]; then
  step "go test -race"
  go test -race ./... || record "go test"
else
  step "go test"
  go test ./... || record "go test"
fi

step "contract (apigen -check)"
go run ./tools/apigen -check || record "contract"

step "docs (links, index counts, md/html mirrors)"
sh scripts/docs-check.sh || record "docs"

printf '\n'
if [ "$fail" -ne 0 ]; then
  printf 'VERIFY FAILED: %s\n' "${failed_steps[*]}"
  exit 1
fi
printf 'VERIFY OK\n'
