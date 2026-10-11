package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/spike"
)

const devUsage = "usage: brw dev keg-create|keg-remove|hold-lock [flags]"

// dev runs the commands used to test a real prefix.
func dev(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(devUsage)
	}
	cmd := args[0]
	fs := newFlagSet("dev " + cmd)
	prefix := fs.String("prefix", "/opt/homebrew", "Homebrew prefix")
	name := fs.String("name", "brw-spike-test", "formula name")
	version := fs.String("version", "0.0.1", "keg version")
	journal := fs.String("journal", "", "append changes to this file")

	switch cmd {
	case "keg-create", "keg-remove", "hold-lock":
	default:
		return fmt.Errorf("unknown dev command %q\n%s", cmd, devUsage)
	}
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
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
		if _, err := fmt.Fprintln(out, "locked"); err != nil {
			return errors.Join(err, l.Release())
		}
		_, _ = bufio.NewReader(os.Stdin).ReadByte() // hold until stdin closes
		return l.Release()
	})
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
