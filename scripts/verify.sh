#!/usr/bin/env bash
# verify — the single gate for this repo (Linux / macOS / CI).
#
#   ./scripts/verify.sh          # gofmt + vet + build + staticcheck + govulncheck + test + coverage + contract + docs
#   RACE=1 ./scripts/verify.sh   # same, with the race detector
'
@'
#   ./scripts/verify.sh          # gofmt + vet + build + staticcheck + govulncheck + test + coverage + contract + docs
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

# staticcheck and govulncheck are installed by the operator, not silently by this script:
# a gate that downloads a tool mid-run is a gate that changes what it checks on its own.
step "staticcheck"
if command -v staticcheck >/dev/null 2>&1; then
  staticcheck ./... || record "staticcheck"
else
  printf 'staticcheck not installed (go install honnef.co/go/tools/cmd/staticcheck@v0.8.2); skipped\n'
fi

step "govulncheck"
if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./... || record "govulncheck"
else
  printf 'govulncheck not installed (go install golang.org/x/vuln/cmd/govulncheck@v1.9.0); skipped\n'
fi

step "go build"
go build ./... || record "go build"

if [ "${RACE:-0}" = "1" ]; then
  step "go test -race"
  go test -race -coverprofile="$cache_root/cover.out" ./... || record "go test"
else
  step "go test"
  go test -coverprofile="$cache_root/cover.out" ./... || record "go test"
fi

step "coverage floor"
if [ -n "${BACKEND_TEST_DATABASE_URL:-}" ]; then
  sh scripts/coverage-check.sh "$cache_root/cover.out" || record "coverage"
else
  # Without the database the equivalence suites skip, so the number is not comparable with
  # CI's. Report it and let CI enforce the floor instead of failing a local run for it.
  printf 'no BACKEND_TEST_DATABASE_URL: reporting coverage only (CI enforces the floor)\n'
  COVERAGE_MIN=0 sh scripts/coverage-check.sh "$cache_root/cover.out" || record "coverage"
fi

step "contract (apigen -check)"
go run ./tools/apigen -check || record "contract"

step "docs (links, index counts, md/html mirrors)"
sh scripts/docs-check.sh || record "docs"

step "makefile parses"
if command -v make >/dev/null 2>&1; then
  make -n help verify test race fmt vet build contract run faked clean >/dev/null 2>&1 ||
    record "makefile"
else
  printf 'make not installed; skipped (CI parses it)\n'
fi

printf '\n'
if [ "$fail" -ne 0 ]; then
  printf 'VERIFY FAILED: %s\n' "${failed_steps[*]}"
  exit 1
fi
printf 'VERIFY OK\n'
