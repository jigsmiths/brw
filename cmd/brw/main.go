package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/selfupdate"
)

const usage = "usage: brw list|search|info|install|reinstall|upgrade|uninstall|autoremove|leaves|link|unlink|cleanup|pin|unpin|version|update|deps|uses|outdated|tag|dev [flags]"

// env is everything a command needs from the outside world.
type env struct {
	out, err io.Writer
	tty      bool // out is a terminal
	host     platform.Host
	client   *api.Client

	// selfUpdate brings brw up to the latest release; nil skips that.
	selfUpdate func(context.Context) (selfupdate.Result, error)

	defaultPrefix string // overrides the host's default prefix, for tests
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
		os.Exit(2)
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
	return &env{out: os.Stdout, err: os.Stderr, tty: isTerminal(os.Stdout), host: host, selfUpdate: selfUpdateBrw}
}

func run(e *env, cmd string, args []string) error {
	switch cmd {
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
	case "tag":
		tag, err := platform.Tag(e.host)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(e.out, tag)
		return err
	case "dev":
		return dev(args, e.out)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
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
