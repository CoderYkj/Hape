#!/bin/sh
# nakama-version-check.sh - the three versions that must move together (ADR 0011).
#
#   1. the Nakama release the image is built from      -> nakama/Dockerfile ARG
#   2. the tag compose actually runs                   -> deploy/docker-compose.yml
#   3. the nakama-common the plugin compiles against   -> nakama/modules/go.mod
#
# (3) is not a number someone typed into a table: it is read from the Nakama release's own
# go.mod, fetched from a module proxy. Bumping Nakama is still a human decision; this check
# only makes forgetting one of the three fail here instead of at plugin load time on a server.
#
#   sh scripts/nakama-version-check.sh                     # source consistency (CI: nakama-module)
#   sh scripts/nakama-version-check.sh --image-tag         # print the image tag to run
#   sh scripts/nakama-version-check.sh --buildinfo FILE    # + the built plugin's recorded dep
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
dockerfile="$root/nakama/Dockerfile"
compose="$root/deploy/docker-compose.yml"
gomod="$root/nakama/modules/go.mod"

fail() {
  printf 'nakama-version-check: %s\n' "$*" >&2
  exit 1
}

[ -f "$dockerfile" ] || fail "missing $dockerfile"
[ -f "$compose" ] || fail "missing $compose"
[ -f "$gomod" ] || fail "missing $gomod"

nakama_version=$(sed -n 's/^ARG NAKAMA_VERSION=\([0-9][0-9.]*\)$/\1/p' "$dockerfile" | head -n 1)
compose_version=$(sed -n 's/.*NAKAMA_VERSION:-\([0-9][0-9.]*\).*/\1/p' "$compose" | head -n 1)
pinned_common=$(sed -n 's/^require github\.com\/heroiclabs\/nakama-common \(v[0-9][0-9.]*\)$/\1/p' "$gomod" | head -n 1)

[ -n "$nakama_version" ] || fail "nakama/Dockerfile has no 'ARG NAKAMA_VERSION=<x.y.z>'"
[ -n "$compose_version" ] || fail "deploy/docker-compose.yml does not default NAKAMA_VERSION"
[ -n "$pinned_common" ] || fail "nakama/modules/go.mod does not require nakama-common"

if [ "$nakama_version" != "$compose_version" ]; then
  fail "compose runs Nakama $compose_version but the image builds $nakama_version"
fi

# --image-tag is offline: callers need the tag before fetching anything.
if [ "${1:-}" = "--image-tag" ]; then
  printf 'hape-nakama:%s\n' "$nakama_version"
  exit 0
fi

# The authoritative mapping, straight from the release's own go.mod.
fetch_release_gomod() {
  proxies=${NAKAMA_VERSION_PROXIES:-${GOPROXY:-https://proxy.golang.org} https://proxy.golang.org https://goproxy.cn}
  for proxy in $proxies; do
    case "$proxy" in
      *,*) proxy=${proxy%%,*} ;;
    esac
    case "$proxy" in
      "" | off | direct) continue ;;
    esac
    url="$proxy/github.com/heroiclabs/nakama/v3/@v/v$nakama_version.mod"
    if body=$(curl -fsSL --max-time 25 "$url" 2>/dev/null); then
      printf '%s' "$body"
      return 0
    fi
  done
  return 1
}

release_gomod=$(fetch_release_gomod) ||
  fail "could not fetch the go.mod of Nakama $nakama_version from any module proxy (needs network)"

release_common=$(printf '%s\n' "$release_gomod" |
  sed -n 's/.*github\.com\/heroiclabs\/nakama-common \(v[0-9][0-9.]*\).*/\1/p' | head -n 1)
[ -n "$release_common" ] || fail "Nakama $nakama_version's go.mod does not mention nakama-common"

if [ "$release_common" != "$pinned_common" ]; then
  fail "Nakama $nakama_version needs nakama-common $release_common, but nakama/modules/go.mod pins $pinned_common"
fi

# --buildinfo checks the artifact that will actually be loaded, not just the source.
if [ "${1:-}" = "--buildinfo" ]; then
  buildinfo=${2:-}
  [ -n "$buildinfo" ] || fail "--buildinfo needs a file path"
  [ -f "$buildinfo" ] || fail "no such buildinfo file: $buildinfo"
  if ! grep -Eq "nakama-common[[:space:]]+$pinned_common" "$buildinfo"; then
    printf 'nakama-version-check: the built plugin does not record nakama-common %s\n' "$pinned_common" >&2
    grep -i 'nakama-common' "$buildinfo" >&2 || true
    exit 1
  fi
fi

printf 'nakama-version-check: OK nakama=%s nakama-common=%s\n' "$nakama_version" "$pinned_common"