// Command brw is a Homebrew-compatible package client.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/tap"
)

const usage = `Example usage:
  brw search TEXT|/REGEX/
  brw info [FORMULA|CASK...]
  brw install FORMULA|CASK...
  brw reinstall FORMULA...
  brw update
  brw upgrade [FORMULA|CASK...]
  brw uninstall FORMULA|CASK...
  brw autoremove
  brw list [FORMULA|CASK...]
  brw leaves
  brw outdated
  brw deps FORMULA
  brw uses FORMULA
  brw link FORMULA...
  brw unlink FORMULA...
  brw pin FORMULA...
  brw unpin FORMULA...
  brw cleanup [FORMULA|CASK...]
  brw trust [TAP|CASK...]
  brw untrust TAP|CASK...
  brw version
  brw tag

Further help:
  brw help
  brw COMMAND -h`

// env is everything a command needs from the outside world.
type env struct {
	out, err io.Writer
	tty      bool // out is a terminal
	host     platform.Host
	client   *api.Client

	defaultPrefix string // overrides the host's default prefix, for tests
	// tapRemote overrides where a tap is cloned from, for tests.
	tapRemote func(name string) string
	// pull overrides how update pulls a tap, for tests.
	pull func(ctx context.Context, t tap.Tap) (changed bool, err error)
}

// kegs lists the kegs of a prefix and warns about each receipt it could not read.
func (e *env) kegs(prefix string) ([]cellar.Keg, error) {
	kegs, err := cellar.Prefix(prefix).Kegs()
	for _, k := range kegs {
		if k.ReceiptErr != nil {
			_, _ = fmt.Fprintf(e.err, "brw: warning: %s %s: %v\n", k.Name, k.Version, k.ReceiptErr)
		}
	}
	return kegs, err
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(1)
	}
	host := platform.Current(macVersion())
	e := newEnv(host)
	if err := run(e, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "brw:", err)
		os.Exit(1)
	}
}

// newEnv is the environment of the real brw.
func newEnv(host platform.Host) *env {
	return &env{out: os.Stdout, err: os.Stderr, tty: isTerminal(os.Stdout), host: host}
}

func run(e *env, cmd string, args []string) error {
	switch cmd {
	case "help", "--help", "-h":
		_, err := fmt.Fprintln(e.out, usage)
		return err
	case "list":
		return list(e, args)
	case "search":
		return searchCommand(e, args)
	case "deps":
		return depsCommand(e, args)
	case "uses":
		return usesCommand(e, args)
	case "outdated":
		return outdatedCommand(e, args)
	case "info":
		return infoCommand(e, args)
	case "install":
		return installCommand(e, args)
	case "reinstall":
		return reinstallCommand(e, args)
	case "upgrade":
		return upgradeCommand(e, args)
	case "leaves":
		return leavesCommand(e, args)
	case "uninstall":
		return uninstallCommand(e, args)
	case "autoremove":
		return autoremoveCommand(e, args)
	case "link":
		return linkCommand(e, args)
	case "unlink":
		return unlinkCommand(e, args)
	case "cleanup":
		return cleanupCommand(e, args)
	case "pin":
		return pinCommand(e, args)
	case "unpin":
		return unpinCommand(e, args)
	case "version":
		return versionCommand(e, args)
	case "update":
		return updateCommand(e, args)
	case "trust":
		return trustCommand(e, args)
	case "untrust":
		return untrustCommand(e, args)
	case "tag":
		tag, err := platform.Tag(e.host)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(e.out, tag)
		return err
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func withRoot(prefix, journalPath string, fn func(*fsx.Root) error) (err error) {
	var journal io.Writer
	if journalPath != "" {
		f, openErr := os.OpenFile(journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //fsx:allow journal lives outside the prefix
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, f.Close()) }()
		journal = f
	}
	root, err := fsx.Open(prefix, journal)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, root.Close()) }()
	return fn(root)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// defaultPrefix is where Homebrew installs on this host.
func defaultPrefix(h platform.Host) string {
	switch {
	case h.OS == "linux":
		return "/home/linuxbrew/.linuxbrew"
	case h.Arch == "arm64":
		return "/opt/homebrew"
	}
	return "/usr/local"
}
