package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/racks"
)

func linkCommand(e *env, args []string) error {
	fs := newFlagSet("link")
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	var force, dryRun, overwrite bool
	fs.BoolVar(&force, "force", false, "link a keg-only formula")
	fs.BoolVar(&force, "f", false, "link a keg-only formula")
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be linked")
	fs.BoolVar(&dryRun, "n", false, "show what would be linked")
	fs.BoolVar(&overwrite, "overwrite", false, "replace files that are in the way")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw link [--force] [--dry-run] FORMULA [FORMULA ...]")
	}
	if overwrite {
		return errors.New("--overwrite is not supported yet")
	}
	return linkOrUnlink(e, names, *prefixArg, *journal, dryRun, func(l *linker, k cellar.Keg) error {
		return l.link(k, force)
	}, func(finder racks.Finder, rack string) cellar.Keg { return cellar.Latest(finder.Installed(rack)) })
}

func unlinkCommand(e *env, args []string) error {
	fs := newFlagSet("unlink")
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be unlinked")
	fs.BoolVar(&dryRun, "n", false, "show what would be unlinked")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw unlink [--dry-run] FORMULA [FORMULA ...]")
	}
	return linkOrUnlink(e, names, *prefixArg, *journal, dryRun, (*linker).unlink,
		func(finder racks.Finder, rack string) cellar.Keg {
			return cellar.Prefix(finder.Prefix).DefaultKeg(finder.Installed(rack))
		})
}

// linkOrUnlink finds the kegs for the names, all before changing anything, and
// applies do to each.
func linkOrUnlink(e *env, names []string, prefixArg, journal string, dryRun bool, do func(*linker, cellar.Keg) error,
	pick func(racks.Finder, string) cellar.Keg) (err error) {
	prefix, err := filepath.Abs(prefixArg)
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
			return name, true // no listing: report the missing keg
		}
		f, ok := idx.Lookup(name)
		if !ok {
			return name, false
		}
		return f.Name, true
	}}
	var chosen []cellar.Keg
	var problems []error
	for _, n := range names {
		rack, err := finder.Find(n)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if k := pick(finder, rack); !slices.ContainsFunc(chosen, func(x cellar.Keg) bool { return x.Path == k.Path }) {
			chosen = append(chosen, k)
		}
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	run := func(root *fsx.Root) error {
		l := &linker{e: e, root: root, prefix: prefix, dry: dryRun, index: index,
			linker: &link.Linker{Root: root, Prefix: prefix}}
		var failed []error
		for _, k := range chosen {
			if err := do(l, k); err != nil {
				failed = append(failed, err)
			}
		}
		return errors.Join(failed...)
	}
	if dryRun {
		root, openErr := fsx.OpenDryRun(prefix, e.out)
		if openErr != nil {
			return openErr
		}
		defer func() { err = errors.Join(err, root.Close()) }()
		return run(root)
	}
	return withRoot(prefix, journal, run)
}

type linker struct {
	e      *env
	root   *fsx.Root
	prefix string
	dry    bool
	index  func() (*api.Index, error)
	linker *link.Linker
}

func (l *linker) keg(k cellar.Keg) link.Keg {
	lk := link.Keg{Name: k.Name, Version: k.Version}
	if k.Receipt != nil {
		lk.Aliases = k.Receipt.Aliases
	}
	return lk
}

func (l *linker) kegPath(k cellar.Keg) string {
	return filepath.Join(l.prefix, "Cellar", k.Name, k.Version)
}

func (l *linker) link(k cellar.Keg, force bool) error {
	lk := l.keg(k)
	if l.linker.Linked(lk) {
		l.warn("Already linked: %s", l.kegPath(k))
		hint := "brw link " + k.Name
		if f, ok := l.formula(k.Name); ok && f.KegOnly && f.KegOnlyReason.Reason != versionedReason {
			hint = "brw link --force " + k.Name
		}
		_, _ = fmt.Fprintf(l.e.out, "To relink, run:\n  brw unlink %s && %s\n", k.Name, hint)
		return nil
	}
	f, known := l.formula(k.Name)
	if !known && !force {
		return fmt.Errorf("cannot tell whether %s is keg-only without the formula listing; use --force to link it anyway", k.Name)
	}
	kegOnly := known && f.KegOnly
	versioned := known && f.KegOnlyReason.Reason == versionedReason
	if kegOnly && !versioned {
		if l.prefix == l.e.hostPrefix() && providedByMacOS(f.KegOnlyReason.Reason) {
			l.warn("Refusing to link macOS provided/shadowed software: %s", k.Name)
			return nil
		}
		if !force {
			l.warn("%s is keg-only and must be linked with `--force`.", k.Name)
			l.pathHint(k)
			return nil
		}
	}
	if l.dry {
		_, _ = fmt.Fprintf(l.e.out, "Would link %s:\n", l.kegPath(k))
		_, err := l.linker.LinkCount(lk)
		return err
	}
	unlock, err := l.lock(k.Name)
	if err != nil {
		return err
	}
	defer unlock()
	_, _ = fmt.Fprintf(l.e.out, "Linking %s... ", l.kegPath(k))
	n, err := l.linker.LinkCount(lk)
	if err != nil {
		_, _ = fmt.Fprintln(l.e.out)
		return fmt.Errorf("%s: %w", k.Name, err)
	}
	_, _ = fmt.Fprintf(l.e.out, "%d symlinks created.\n", n)
	if kegOnly && !versioned {
		l.pathHint(k)
	}
	return nil
}

func (l *linker) unlink(k cellar.Keg) error {
	lk := l.keg(k)
	if l.dry {
		_, _ = fmt.Fprintf(l.e.out, "Would unlink %s:\n", l.kegPath(k))
		_, err := l.linker.UnlinkCount(lk)
		return err
	}
	unlock, err := l.lock(k.Name)
	if err != nil {
		return err
	}
	defer unlock()
	_, _ = fmt.Fprintf(l.e.out, "Unlinking %s... ", l.kegPath(k))
	n, err := l.linker.UnlinkCount(lk)
	if err != nil {
		_, _ = fmt.Fprintln(l.e.out)
		return fmt.Errorf("%s: %w", k.Name, err)
	}
	_, _ = fmt.Fprintf(l.e.out, "%d symlinks removed.\n", n)
	return nil
}

func (l *linker) lock(name string) (func(), error) {
	lk, err := lock.Acquire(l.root, name)
	if err != nil {
		return nil, err
	}
	return func() { _ = lk.Release() }, nil
}

func (l *linker) formula(name string) (*api.Formula, bool) {
	idx, err := l.index()
	if err != nil {
		return nil, false
	}
	return idx.Lookup(name)
}

func (l *linker) warn(format string, args ...any) {
	_, _ = fmt.Fprintf(l.e.err, "brw: warning: "+format+"\n", args...)
}

// pathHint says how to use a keg-only formula without linking it.
func (l *linker) pathHint(k cellar.Keg) {
	var dirs []string
	for _, d := range []string{"bin", "sbin"} {
		if fi, err := os.Stat(filepath.Join(l.kegPath(k), d)); err == nil && fi.IsDir() {
			dirs = append(dirs, filepath.Join(l.prefix, "opt", k.Name, d))
		}
	}
	if len(dirs) == 0 {
		return
	}
	_, _ = fmt.Fprintf(l.e.out, "\nTo use it without linking it, put %s first in your PATH.\n", strings.Join(dirs, " and "))
}

const versionedReason = ":versioned_formula"

// hostPrefix is where Homebrew installs on this host.
func (e *env) hostPrefix() string {
	if e.defaultPrefix != "" {
		return e.defaultPrefix
	}
	return defaultPrefix(e.host)
}

// providedByMacOS says whether a keg-only reason is that macOS has its own copy.
func providedByMacOS(reason string) bool {
	return reason == ":provided_by_macos" || reason == ":shadowed_by_macos"
}
