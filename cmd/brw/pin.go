package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/pin"
	"github.com/jigsmiths/brw/internal/racks"
)

func pinCommand(e *env, args []string) error { return pinOrUnpin(e, "pin", args) }

func unpinCommand(e *env, args []string) error { return pinOrUnpin(e, "unpin", args) }

// pinOrUnpin acts on installed formulae as brew does: an unknown name fails
// before anything changes, one that is known but not installed is reported and
// skipped (and fails pin, not unpin), and one in the wanted state gets a warning.
func pinOrUnpin(e *env, verb string, args []string) error {
	fs := newFlagSet(verb)
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return fmt.Errorf("usage: brw %s FORMULA [FORMULA ...]", verb)
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	kegs, err := cellar.Prefix(prefix).Kegs()
	if err != nil {
		return err
	}
	index := sync.OnceValues(func() (*api.Index, error) { return e.index(context.Background()) })
	finder := racks.Finder{Prefix: prefix, Kegs: kegs, Resolve: func(name string) (string, bool) {
		idx, err := index()
		if err != nil {
			return name, true
		}
		f, ok := idx.Lookup(name)
		if !ok {
			return name, false
		}
		return f.Name, true
	}}

	type target struct{ name, rack string } // rack is empty when nothing is installed
	var targets []target
	var unknown []error
	for _, n := range names {
		rack, err := finder.Find(n)
		var missing *racks.NoSuchKegError
		switch {
		case errors.As(err, &missing):
			targets = append(targets, target{name: filepath.Base(missing.Path)})
		case err != nil:
			unknown = append(unknown, err)
		default:
			targets = append(targets, target{name: rack, rack: rack})
		}
	}
	if len(unknown) > 0 {
		return errors.Join(unknown...)
	}

	return withRoot(prefix, *journal, func(root *fsx.Root) error {
		var failed []error
		for _, t := range targets {
			switch {
			case t.rack == "" && verb == "pin":
				failed = append(failed, fmt.Errorf("%s not installed", t.name))
			case t.rack == "":
				_, _ = fmt.Fprintf(e.err, "brw: %s not installed\n", t.name)
			case verb == "pin" && pin.Pinned(prefix, t.rack):
				_, _ = fmt.Fprintf(e.err, "brw: warning: %s already pinned\n", t.name)
			case verb == "unpin" && !pin.Pinned(prefix, t.rack):
				_, _ = fmt.Fprintf(e.err, "brw: warning: %s not pinned\n", t.name)
			case verb == "pin":
				if err := pin.Pin(root, finder.Installed(t.rack)); err != nil {
					failed = append(failed, fmt.Errorf("%s: %w", t.name, err))
				}
			default:
				if err := pin.Unpin(root, t.rack); err != nil {
					failed = append(failed, fmt.Errorf("%s: %w", t.name, err))
				}
			}
		}
		return errors.Join(failed...)
	})
}

// pinnedList is what list --pinned prints: each pinned formula, with the
// version its record names when asked.
func pinnedList(prefix string, versions bool) (string, error) {
	names, err := cellar.Prefix(prefix).Names()
	if err != nil {
		return "", err
	}
	var out []string
	for _, n := range names {
		if !pin.Pinned(prefix, n) {
			continue
		}
		if versions {
			target, err := os.Readlink(filepath.Join(prefix, "var/homebrew/pinned", n))
			if err != nil {
				return "", err
			}
			n += " " + filepath.Base(target)
		}
		out = append(out, n)
	}
	return lines(out), nil
}
