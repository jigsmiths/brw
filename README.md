# brw

[![ci](https://github.com/jigsmiths/brw/actions/workflows/ci.yml/badge.svg)](https://github.com/jigsmiths/brw/actions/workflows/ci.yml)

A Go, Homebrew-compatible package client. It reads Homebrew's metadata and bottles and shares a prefix with `brew`.

**Status:** early. Read-only core only; no install or upgrade yet.

## Usage

```sh
go build ./cmd/brw

brw list [--prefix /opt/homebrew]   # list installed kegs
brw tag                             # bottle tag for this machine
```

## Development

```sh
git config core.hooksPath .githooks
gofmt -l . && golangci-lint run ./... && go test -race ./...
```

Writes to a prefix go through `internal/fsx`. Tests use temp prefixes only.

## License

MIT
