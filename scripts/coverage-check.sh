#!/bin/sh
# coverage-check.sh - the coverage floor, measured on everything except tools/.
#
#   COVERAGE_MIN=58 sh scripts/coverage-check.sh /tmp/cover.out
#
# tools/ are excluded on purpose: fakeds is exercised by the ds job end to end, apigen by
# `-check`, loadtest by hand - but they have no unit tests, so counting them would measure
# the harness rather than the code under test. The floor is on the rest.
#
# The number is a ratchet, not a target: it exists so a change that drops coverage is a
# conversation, not a silent drift. Raise it when it gets comfortable, never lower it to
# make a red build green.
set -eu

profile="${1:?usage: coverage-check.sh <coverprofile>}"
floor="${COVERAGE_MIN:-58}"

[ -f "$profile" ] || {
  echo "coverage-check: no profile at $profile" >&2
  exit 1
}

filtered="$(mktemp)"
trap 'rm -f "$filtered"' EXIT INT TERM
grep -v '^gamebackend/tools/' "$profile" > "$filtered" || true

total="$(go tool cover -func="$filtered" | awk '/^total:/ {print $NF}' | tr -d '%')"
[ -n "$total" ] || {
  echo "coverage-check: could not read a total from $profile" >&2
  exit 1
}

printf 'coverage (non-tool): %s%% (floor %s%%)\n' "$total" "$floor"
awk -v got="$total" -v floor="$floor" 'BEGIN {
  if (got + 0 < floor + 0) {
    printf "coverage-check: %.1f%% is below the %s%% floor\n", got, floor > "/dev/stderr"
    exit 1
  }
}'