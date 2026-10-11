package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/platform"
)

const usage = "usage: brw list|tag|dev [flags]"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "brw:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string, out io.Writer) error {
	switch cmd {
	case "list":
		return list(args, out)
	case "tag":
		tag, err := platform.Tag(platform.Current(macVersion()))
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, tag)
		return err
	case "dev":
		return dev(args, out)
	}
	return fmt.Errorf("unknown command %q\n%s", cmd, usage)
}

func list(args []string, out io.Writer) error {
	fs := newFlagSet("list")
	prefix := fs.String("prefix", "/opt/homebrew", "Homebrew prefix")
	if err := fs.Parse(args); err != nil {
		return err
	}
	kegs, err := cellar.Prefix(*prefix).Kegs()
	if err != nil {
		return err
	}
	_, err = io.WriteString(out, formatVersions(kegs))
	return err
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

// formatVersions matches the output of brew list --versions.
func formatVersions(kegs []cellar.Keg) string {
	var b strings.Builder
	for i, k := range kegs {
		if i == 0 || kegs[i-1].Name != k.Name {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(k.Name)
		}
		b.WriteString(" " + k.Version)
	}
	if len(kegs) > 0 {
		b.WriteByte('\n')
	}
	return b.String()
}
