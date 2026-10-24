.PHONY: build test integration check setup

VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-X main.version=$(VERSION)" -o bin/ghraft ./cmd/ghraft

setup: build
	bash scripts/setup.sh

test:
	go test -race ./...

# Server drivers require a private DSN for a disposable loopback database ending
# in _test. GHRAFT_TEST_DRIVER=sqlite uses isolated temporary files instead.
integration:
	GHRAFT_REQUIRE_DB=1 go test -race -count=1 -v ./...

check:
	go vet ./...
	test -z "$$(gofmt -l cmd internal)"
	git diff --check
	bash -n scripts/setup.sh
