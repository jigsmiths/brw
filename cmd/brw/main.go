package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/spike"
)

const usage = "usage: brw list|tag|keg-create|keg-remove|hold-lock [flags]"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}
	if err := run(os.Args[1], os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "brw:", err)
		os.Exit(1)
	}
}

func run(cmd string, args []string) error {
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	prefix := fs.String("prefix", "/opt/homebrew", "Homebrew prefix")
	name := fs.String("name", "brw-spike-test", "formula name")
	version := fs.String("version", "0.0.1", "keg version")
	journal := fs.String("journal", "", "append changes to this file")
	fs.Parse(args)

	switch cmd {
	case "list":
		kegs, err := cellar.Prefix(*prefix).Kegs()
		if err != nil {
			return err
		}
		fmt.Print(formatVersions(kegs))
	case "tag":
		tag, err := platform.Tag(platform.Current(macVersion()))
		if err != nil {
			return err
		}
		fmt.Println(tag)
	case "keg-create", "keg-remove", "hold-lock":
		return withRoot(*prefix, *journal, func(root *fsx.Root) error {
			switch cmd {
			case "keg-create":
				return spike.CreateKeg(root, *name, *version, time.Now())
			case "keg-remove":
				return spike.RemoveKeg(root, cellar.Prefix(*prefix), *name, *version)
			}
			l, err := lock.Acquire(root, *name)
			if err != nil {
				return err
			}
			defer l.Release()
			fmt.Println("locked")
			bufio.NewReader(os.Stdin).ReadByte() // hold until stdin closes
			return nil
		})
	default:
		return fmt.Errorf("unknown command %q\n%s", cmd, usage)
	}
	return nil
}

func withRoot(prefix, journalPath string, fn func(*fsx.Root) error) error {
	var journal io.Writer
	if journalPath != "" {
		f, err := os.OpenFile(journalPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644) //fsx:allow journal lives outside the prefix
		if err != nil {
			return err
		}
		defer f.Close()
		journal = f
	}
	root, err := fsx.Open(prefix, journal)
	if err != nil {
		return err
	}
	defer root.Close()
	return fn(root)
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
