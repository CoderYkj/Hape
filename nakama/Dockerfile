# The Nakama image with our Go runtime plugin baked in.
#
# Why the plugin is built HERE and not on the host / in CI: a Go plugin shares the hosting
# binary's type information, so plugin.Open compares package hashes. The official
# heroiclabs/nakama binary is built with a specific Go toolchain and `--trimpath`; a .so built
# anywhere else fails with "plugin was built with a different version of package
# internal/goarch" and Nakama refuses to start. heroiclabs/nakama-pluginbuilder:<same tag> is
# the supported way to get a matching toolchain and the matching nakama-common version.
#
#   docker build -t hape-nakama:3.22.0 nakama/
#   docker run --rm hape-nakama:3.22.0 --version
#
# ADR 0011 (supersedes ADR 0009: the Lua bridge was the interim path; the Go plugin is the
# intended one and this is what makes it loadable).
ARG NAKAMA_VERSION=3.22.0

FROM heroiclabs/nakama-pluginbuilder:${NAKAMA_VERSION} AS build

WORKDIR /plugin

# go.mod/go.sum first: the module cache layer survives source edits.
COPY modules/go.mod modules/go.sum ./
RUN go mod download

COPY modules/ ./

# CGO is required for Go plugins. --trimpath must match how the server itself was built
# (Nakama's own Dockerfile uses it) or the package hashes differ.
RUN CGO_ENABLED=1 go build --trimpath --buildmode=plugin -o /out/gamemodule.so .

# Record what the artifact was actually built against. scripts/nakama-version-check.sh reads
# this in CI, so "the plugin and the server agree" is asserted on the .so that ships, not only
# on go.mod.
RUN go version -m /out/gamemodule.so > /out/gamemodule.buildinfo

# ---------------------------------------------------------------------------
# runtime
# ---------------------------------------------------------------------------
FROM heroiclabs/nakama:${NAKAMA_VERSION}

COPY --from=build /out/gamemodule.so /nakama/data/modules/gamemodule.so
COPY --from=build /out/gamemodule.buildinfo /nakama/data/modules/gamemodule.buildinfo