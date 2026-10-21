.PHONY: build test integration check

build:
	go build -trimpath -o bin/ghraft ./cmd/ghraft

test:
	go test -race ./...

# Requires a private DSN file for a disposable loopback database ending in _test.
integration:
	GHRAFT_REQUIRE_DB=1 go test -race -count=1 -v ./...

check:
	go vet ./...
	test -z "$$(gofmt -l cmd internal)"
	git diff --check
