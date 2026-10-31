# Convenience targets for Linux/macOS/CI. Windows: use scripts/verify.ps1.
GO ?= go
export GOFLAGS ?= -buildvcs=false

.PHONY: help verify test race fmt vet build contract run faked clean db-migrate db-probe

help:
	@echo "verify   gofmt + vet + build + test + contract drift"
	@echo "test     go test ./..."
	@echo "race     go test -race ./..."
	@echo "fmt      gofmt -w ."
	@echo "contract go run ./tools/apigen"
	@echo "run      go run ./cmd/platform"
	@echo "faked    go run ./tools/fakeds"
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

# Needs a reachable PostgreSQL: set PGHOST/PGPORT/PGUSER/PGDATABASE (psql reads them).
# Windows: scripts/pg-local.ps1 -Action migrate|probe does the same on the local instance.
db-migrate:
	@for f in db/migrations/*.sql; do echo "applying $$f"; psql -v ON_ERROR_STOP=1 -f "$$f" || exit 1; done

db-probe:
	@for f in db/probes/*.probe.sql; do echo "probing $$f"; psql -t -q -v ON_ERROR_STOP=1 -f "$$f" || exit 1; done

clean:
	rm -rf bin
