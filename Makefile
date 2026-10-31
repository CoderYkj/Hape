# Convenience targets for Linux/macOS/CI. Windows: use scripts/verify.ps1.
GO ?= go
export GOFLAGS ?= -buildvcs=false

.PHONY: help verify test race fmt vet build contract run faked clean

help:
	@echo "verify   gofmt + vet + build + test + contract drift"
	@echo "test     go test ./..."
	@echo "race     go test -race ./..."
	@echo "fmt      gofmt -w ."
	@echo "contract go run ./tools/apigen"
	@echo "run      go run ./cmd/platform"
	@echo "faked    go run ./tools/fakeds"

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

clean:
	rm -rf bin
