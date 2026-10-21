# brw

[![ci](https://github.com/jigsmiths/brw/actions/workflows/ci.yml/badge.svg)](https://github.com/jigsmiths/brw/actions/workflows/ci.yml)

A Go, Homebrew-compatible package client. It reads Homebrew's metadata and bottles and shares a prefix with `brew`.

**Status:** early. Queries, install, upgrade, reinstall, uninstall, link, unlink and cleanup work for bottled formulae.

## Install

```sh
brew install --cask jigsmiths/tap/brw
```

## Usage

Needs [just](https://github.com/casey/just) and Go.

```sh
just build                       # build bin/brw
just run list                    # installed formulae (--versions for versions)
just run search jq               # also: search --desc "json processor", search '/^lib/'
just run info jq                 # also: info --json jq
just run deps git --tree         # also: --1, --installed
just run uses --installed pcre2
just run leaves                  # not needed by anything else; -r on request, -p as dependency
just run outdated                # also: --json, --quiet
just run update                  # refresh the formula listing and update brw (--no-self to skip that)
just install                     # put brw on your PATH (GOBIN)
```

Commands that change the prefix take `--dry-run` (`-n`) to show what they would do.

```sh
just run install jq              # bottle and its dependencies
just run upgrade                 # all outdated formulae, or name some
just run reinstall jq
just run uninstall jq            # also: --force, --ignore-dependencies
just run autoremove              # drop dependencies nothing needs
just run link jq                 # also: unlink; link --force, --overwrite
just run cleanup                 # also: --prune=30, --scrub, --prune-prefix
```

Commands take `--prefix` and default to Homebrew's prefix for this machine. Formulae only, no casks.

## Development

```sh
just setup          # once: point git at the repo hooks
just check          # gofmt, lint and tests, as CI and the pre-commit hook run them
just test           # tests only
just lint
just fmt
just conformance    # compare the read-only commands with the real brew (slow)
just conformance-install    # install with brew and brw, compare the prefixes
just conformance-uninstall  # the same for upgrade, reinstall, cleanup, link and uninstall
just clean
```

Writes to a prefix go through `internal/fsx`. Tests use temp prefixes only. The two install harnesses need a brew checkout at `~/.brw-oracle/hb`.

## License

MIT
