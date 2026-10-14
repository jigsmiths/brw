# brw

[![ci](https://github.com/jigsmiths/brw/actions/workflows/ci.yml/badge.svg)](https://github.com/jigsmiths/brw/actions/workflows/ci.yml)

A Go, Homebrew-compatible package client. It reads Homebrew's metadata and bottles and shares a prefix with `brew`.

**Status:** early. The read-only commands work; no install or upgrade yet.

## Usage

Needs [just](https://github.com/casey/just) and Go.

```sh
just build                       # build bin/brw
just run list                    # installed formulae (--versions for versions)
just run search jq               # also: search --desc "json processor", search '/^lib/'
just run info jq                 # also: info --json jq
just run deps git --tree         # also: --1, --installed
just run uses --installed pcre2
just run outdated                # also: --json, --quiet
just run update                  # refresh the formula listing now (it otherwise refreshes after 10 minutes)
just install                     # put brw on your PATH (GOBIN)
```

Commands take `--prefix` and default to Homebrew's prefix for this machine. Formulae only, no casks.

## Development

```sh
just setup          # once: point git at the repo hooks
just check          # gofmt, lint and tests, as CI and the pre-commit hook run them
just test           # tests only
just lint
just fmt
just conformance    # compare with the real brew on this machine (slow)
just clean
```

Writes to a prefix go through `internal/fsx`. Tests use temp prefixes only.

## License

MIT
