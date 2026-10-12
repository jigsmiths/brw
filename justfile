default:
    @just --list

# Build bin/brw
build:
    go build -o bin/brw ./cmd/brw

# Run brw with the given arguments
run *args:
    go run ./cmd/brw {{ args }}

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

# Point git at the repo hooks
setup:
    git config core.hooksPath .githooks

clean:
    rm -rf bin

# Install the same formulae with brew and brw, then compare (needs ~/.brw-oracle/hb)
conformance-install:
    BRW_CONFORMANCE_INSTALL=1 go test ./cmd/brw -run TestConformanceInstall -count=1 -v -timeout 20m
