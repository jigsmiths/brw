# brw

[![ci](https://github.com/jigsmiths/brw/actions/workflows/ci.yml/badge.svg)](https://github.com/jigsmiths/brw/actions/workflows/ci.yml)

A Go, Homebrew-compatible package client. It reads Homebrew's metadata and bottles and shares a prefix with `brew`.

**Status:** early. Queries, install, upgrade, reinstall, uninstall, link, unlink and cleanup work for bottled formulae. For casks, the queries work and brw installs, upgrades and uninstalls casks whose artifacts are apps and binaries.

## Install

```sh
brew install --cask jigsmiths/tap/brw
```

## Usage

Needs [just](https://github.com/casey/just) and Go.

```sh
just build                       # build bin/brw
just run list                    # installed formulae and casks (--formula, --cask, --versions)
just run search jq               # also: search --desc "json processor", search '/^lib/', --cask, --formula
just run info jq                 # also: info --json jq, info --cask slack
just run deps git --tree         # also: --1, --installed
just run uses --installed pcre2
just run leaves                  # not needed by anything else; -r on request, -p as dependency
just run outdated                # also: --cask, --formula, --greedy, --json, --quiet
just run update                  # refresh the formula and cask listings and pull the taps
just install                     # put brw on your PATH (GOBIN)
```

Commands that change the prefix take `--dry-run` (`-n`) to show what they would do.

```sh
just run install jq              # bottle and its dependencies
just run install --cask slack    # app and binary casks; also --appdir, --adopt; formulae and casks can be mixed
just run install jigsmiths/tap/brw  # a tap's cask by full name, tapping it first if needed
just run upgrade                 # all outdated formulae and casks, or name some
just run reinstall jq
just run uninstall jq            # also: --cask, --force, --ignore-dependencies
just run autoremove              # drop dependencies nothing needs
just run link jq                 # also: unlink; link --force, --overwrite
just run cleanup                 # also: --prune=30, --scrub, --prune-prefix
```

Update brw itself with `brw update && brw upgrade brw`, or `brew upgrade --cask brw`. Each release signs `checksums.txt` with Sigstore. To check a release by hand:

```sh
cosign verify-blob checksums.txt --bundle checksums.txt.sigstore.json \
  --certificate-identity https://github.com/jigsmiths/brw/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

Commands take `--prefix` and default to Homebrew's prefix for this machine.

### Taps

brw reads a tap's cask files with its own evaluator for the cask DSL, so it needs no Ruby. Simple `preflight` and `postflight` blocks become install steps; brw refuses a cask with Ruby it can't translate. Formulae from taps aren't supported.

Like brew, brw only loads casks from taps you trust, and shares brew's trust store (`$XDG_CONFIG_HOME/homebrew/trust.json`, else `~/.homebrew/trust.json`). `brw trust jigsmiths/tap` trusts a tap, `brw untrust` removes it, and `brw trust` lists what's trusted. Installing a cask by its full name trusts that cask. `HOMEBREW_NO_REQUIRE_TAP_TRUST=1` turns the check off.

## Development

```sh
just setup          # once: use the repo's git hooks
just check          # what CI and the pre-commit hook run: gofmt, go fix, lint, tests
just test
just lint
just fmt
just conformance    # compare the read-only commands with brew (slow)
just conformance-install      # and -uninstall, -upgrade, -cask-install, -tap, -full
just clean
```

Writes to a prefix go through `internal/fsx`. Tests use temp prefixes only. The conformance harnesses need a brew checkout at `~/.brw-oracle/hb`.

## License

MIT
