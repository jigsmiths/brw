set positional-arguments

default:
    @just --list

# Build bin/brw
build:
    go build -o bin/brw ./cmd/brw

# Run brw with the given arguments
run *args:
    go run ./cmd/brw "$@"

# Install brw into GOBIN
install:
    go install ./cmd/brw

test:
    go test -race ./...

lint:
    golangci-lint run ./...

fmt:
    gofmt -w .

# Same checks as CI and the pre-commit hook
check:
    test -z "$(gofmt -l .)"
    just lint
    just test

# Compare the read-only commands with the real brew on this machine (slow)
conformance:
    BRW_CONFORMANCE=1 go test ./cmd/brw -run Conformance -v -timeout 30m

# Point git at the repo hooks
setup:
    git config core.hooksPath .githooks

clean:
    rm -rf bin
