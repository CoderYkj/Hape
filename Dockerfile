# syntax=docker/dockerfile:1
#
# NOT VERIFIED: written on a machine without Docker (`docker version` fails), so
# this file has never been built. The Go build it runs is the same one
# scripts/verify.sh runs, but the image layers are unproven. Expect to adjust
# base image tags on the first real build.
#
#   docker build -t gamebackend:local .
#   docker run --rm -p 8080:8080 gamebackend:local

# ---------------------------------------------------------------------------
# build
# ---------------------------------------------------------------------------
ARG GO_IMAGE=golang:1.27-alpine
FROM ${GO_IMAGE} AS build

WORKDIR /src

# go.mod first: it is the only input to `go mod download`, so the module cache
# layer survives every source edit. Today there are zero dependencies; the layer
# is here so adding one does not invalidate the whole build.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# - CGO_ENABLED=0: the binary must run on a base image with no toolchain.
# - -buildvcs=false: the build context has no .git (see .dockerignore), and a
#   missing VCS stamp would otherwise fail the build.
# - -trimpath: no local paths baked into the binary.
RUN CGO_ENABLED=0 GOOS=linux go build -buildvcs=false -trimpath \
      -ldflags="-s -w" \
      -o /out/platform ./cmd/platform

# ---------------------------------------------------------------------------
# runtime
# ---------------------------------------------------------------------------
FROM alpine:3.24 AS runtime

# Non-root by default: nothing in this process needs to write to the filesystem.
RUN apk add --no-cache ca-certificates tzdata \
 && adduser -D -H -u 10001 -s /sbin/nologin app

COPY --from=build /out/platform /usr/local/bin/platform

USER 10001:10001

EXPOSE 8080

# Defaults only. Override with -e at run time; BACKEND_ENV=prod additionally
# requires BACKEND_SERVICE_TOKEN and refuses dev auth (see pkg/config).
ENV BACKEND_ADDR=0.0.0.0:8080 \
    BACKEND_ENV=dev \
    BACKEND_ALLOCATOR=local \
    LOG_LEVEL=info

# Liveness only: it must not depend on anything downstream. An orchestrator
# should gate traffic on /readyz instead. If you change BACKEND_ADDR, change the
# port here too - the two are deliberately not coupled through shell expansion,
# because a broken health check is worse than a duplicated constant.
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz > /dev/null || exit 1

ENTRYPOINT ["/usr/local/bin/platform"]