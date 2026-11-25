# brw

[![ci](https://github.com/jigsmiths/brw/actions/workflows/ci.yml/badge.svg)](https://github.com/jigsmiths/brw/actions/workflows/ci.yml)

brw is a Homebrew-compatible package client written in Go. It shares a prefix with `brew`, so you can switch between them for everyday installs and upgrades.

- Install, upgrade and uninstall formulae and casks.
- Tap formulae install from a bottle, or build from source on macOS like brew.
- Reads tap files with its own Ruby evaluator, so it needs no Ruby.
- Uses brew's tap trust store.

brw is early. Casks work when their artifacts are apps, binaries, pkgs and installer scripts, and most uninstall directives. Like brew, brw refuses to run as root, except for `brw services`.

## Install

```sh
brew install --cask jigsmiths/tap/brw
```

Then use it like brew:

```sh
brw install jq
brw upgrade
```

Run `brw help` to see every command. Commands that change the prefix take `--dry-run`.

## Build from source

You need Go and [just](https://github.com/casey/just).

```sh
just setup     # once: use the repo's git hooks
just check     # what CI runs: format, lint, tests
just run ARGS  # run brw from the checkout
```

## License

MIT
