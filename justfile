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
