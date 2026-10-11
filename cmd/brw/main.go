package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/platform"
)

const usage = "usage: brw list|search|deps|uses|outdated|tag|dev [flags]"

// env is everything a command needs from the outside world.
type env struct {
	out, err io.Writer
	tty      bool // out is a terminal
	host     platform.Host
	client   *api.Client
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	host := platform.Current(macVersion())
	e := &env{out: os.Stdout, err: os.Stderr, tty: isTerminal(os.Stdout), host: host}
	if err := run(e, os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "brw:", err)
		os.Exit(1)
	}
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

func macVersion() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	out, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
