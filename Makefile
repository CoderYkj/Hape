# Convenience targets for machines with `make`. The scripts in scripts/ are the source of
# truth and CI runs those directly (Windows users have no make at all, so anything only
# reachable through this file would be unavailable to them).
#
# CI parses this file (`make -n <targets>`) on every push so a typo here cannot rot quietly.
GO ?= go
export GOFLAGS ?= -buildvcs=false

.PHONY: help verify test race fmt vet build contract run faked ds ds-docker clean db-migrate db-probe

help:
	@echo "verify   gofmt + vet + build + test + contract drift"
	@echo "test     go test ./..."
	@echo "race     go test -race ./..."
	@echo "fmt      gofmt -w ."
	@echo "contract go run ./tools/apigen"
	@echo "run      go run ./cmd/platform"
	@echo "faked    go run ./tools/fakeds"
	@echo "ds       build ds/ and run it as a provider-launched server (credentials in env)"
	@echo "ds-docker  same, as a container (needs docker + a built hape-ds:ci image)"
	@echo "db-migrate  apply db/migrations/*.sql with psql (needs PGHOST/PGDATABASE)"
	@echo "db-probe    assert db/probes/*.probe.sql invariants"

verify:
	./scripts/verify.sh

test:
	$(GO) test ./...

race:
	RACE=1 ./scripts/verify.sh

fmt:
	gofmt -w .

vet:
	$(GO) vet ./...

build:
	$(GO) build ./...

contract:
	$(GO) run ./tools/apigen

run:
	$(GO) run ./cmd/platform

faked:
	$(GO) run ./tools/fakeds

# The drill that proves the per-session credential reaches the instance: allocate a
# session, then start ds/ with that credential in its environment (spec 0012).
ds:
	$(GO) run ./tools/fakeds -launch-ds exec

ds-docker:
	$(GO) run ./tools/fakeds -launch-ds docker -ds-image hape-ds:ci

# Needs a reachable PostgreSQL: set PGHOST/PGPORT/PGUSER/PGDATABASE (psql reads them).
# Windows: scripts/pg-local.ps1 -Action migrate|probe does the same on the local instance.
db-migrate:
	@for f in db/migrations/*.sql; do echo "applying $$f"; psql -v ON_ERROR_STOP=1 -f "$$f" || exit 1; done

db-probe:
	@for f in db/probes/*.probe.sql; do echo "probing $$f"; psql -t -q -v ON_ERROR_STOP=1 -f "$$f" || exit 1; done

clean:
	rm -rf bin
