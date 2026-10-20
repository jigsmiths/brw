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

# Install the same formulae with brew and brw, then compare (needs ~/.brw-oracle/hb)
conformance-install:
    BRW_CONFORMANCE_INSTALL=1 go test ./cmd/brw -run TestConformanceInstall -count=1 -v -timeout 20m

# Remove formulae with brew and with brw and compare the prefixes (needs ~/.brw-oracle/hb)
conformance-uninstall:
    BRW_CONFORMANCE_UNINSTALL=1 go test ./cmd/brw -run TestConformanceUninstall -count=1 -v -timeout 20m

# Age kegs in two prefixes, then compare brew and brw update and upgrade (needs ~/.brw-oracle/hb)
conformance-upgrade:
    BRW_CONFORMANCE_UPGRADE=1 go test ./cmd/brw -run TestConformanceUpgrade -count=1 -v -timeout 30m

# Install everything brew has installed here into a scratch prefix and check it (needs ~/.brw-oracle/hb)
conformance-full:
    BRW_CONFORMANCE_FULL=1 go test ./cmd/brw -run TestConformanceFull -count=1 -v -timeout 60m

# Same in a Debian container with the standard Linux prefix (needs Docker)
conformance-linux:
    #!/usr/bin/env bash
    set -euo pipefail
    arch=$(docker run --rm debian:stable-slim dpkg --print-architecture)
    out=$(mktemp -d)
    GOOS=linux GOARCH=$arch go build -o "$out/brw" ./cmd/brw
    GOOS=linux GOARCH=$arch go test -c -o "$out/full.test" ./cmd/brw
    brew list --formula -1 > "$out/names" 2>/dev/null
    docker run --rm -v "$out:/work" -e BRW_CONFORMANCE_FULL=1 -e BRW_BIN=/work/brw \
      -e BRW_FULL_NAMES=/work/names -e BRW_FULL_REPORT=/work/report.txt debian:stable-slim bash -c '
        apt-get update -qq && apt-get install -y -qq ca-certificates >/dev/null
        useradd -m linuxbrew && mkdir -p /home/linuxbrew/.linuxbrew && chown linuxbrew /home/linuxbrew/.linuxbrew /work
        su linuxbrew -c "/work/full.test -test.run TestConformanceFull -test.v -test.timeout 60m"' || status=$?
    cp "$out/report.txt" ~/.brw-oracle/full-report-linux.txt
    rm -rf "$out"
    exit ${status:-0}
