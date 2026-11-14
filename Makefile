.PHONY: build test integration check setup

VERSION ?= $(shell git describe --always --dirty 2>/dev/null || echo dev)
# The sequential database/worktree regression suite can exceed Go's 10m default.
TEST_TIMEOUT ?= 20m

build:
	CGO_ENABLED=0 go build -trimpath -ldflags="-X main.version=$(VERSION)" -o bin/ghraft ./cmd/ghraft

setup: build
	bash scripts/setup.sh

test:
	python3 -m unittest discover -s tests -p 'test_*.py'
	go test -race -timeout $(TEST_TIMEOUT) ./...

# Server drivers require a private DSN for a disposable loopback database ending
# in _test. GHRAFT_TEST_DRIVER=sqlite uses isolated temporary files instead.
integration:
	python3 -m unittest discover -s tests -p 'test_*.py'
	GHRAFT_REQUIRE_DB=1 go test -race -timeout $(TEST_TIMEOUT) -count=1 -v ./...

check:
	go vet ./...
	test -z "$$(gofmt -l cmd internal)"
	git diff --check
	bash -n scripts/setup.sh
