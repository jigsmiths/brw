package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cask"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/uninstall"
)

func uninstallCommand(e *env, args []string) error {
	fs := newFlagSet("uninstall")
	var dryRun bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
	var o uninstall.Options
	fs.BoolVar(&o.Force, "force", false, "remove every installed version")
	fs.BoolVar(&o.Force, "f", false, "remove every installed version")
	fs.BoolVar(&o.IgnoreDependencies, "ignore-dependencies", false, "remove even if installed formulae need it")
	kinds := kindFlags(fs)
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw uninstall [--formula|--cask] [--force] [--ignore-dependencies] [--dry-run] NAME [NAME ...]")
	}
	formulaOnly, caskOnly, err := kinds()
	if err != nil {
		return err
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return err
	}
	installedCasks, err := caskroom.Installed(prefix)
	if err != nil {
		return err
	}
	// As brew does: an installed formula first, then an installed cask.
	var formulae []string
	var casks []caskroom.Cask
	var notInstalled []error
	for _, n := range names {
		i := slices.IndexFunc(installedCasks, func(c caskroom.Cask) bool { return c.Token == n })
		isKeg := slices.ContainsFunc(kegs, func(k cellar.Keg) bool { return k.Name == n })
		switch {
		case formulaOnly, !caskOnly && (isKeg || i < 0):
			formulae = append(formulae, n) // the remover reports one that is not installed
		case i >= 0:
			casks = append(casks, installedCasks[i])
		case !o.Force:
			notInstalled = append(notInstalled, cask.NotInstalled(n))
		}
	}
	return withRoot(prefix, *journal, func(root *fsx.Root) error {
		o.Autoremove = os.Getenv("HOMEBREW_NO_AUTOREMOVE") == ""
		remover := newRemover(e, root, prefix, kegs, dryRun)
		if len(casks) == 0 && len(notInstalled) == 0 {
			return remover.Run(formulae, o)
		}
		// brew stops when a formula fails, and autoremoves after the casks.
		autoremove := o.Autoremove
		o.Autoremove = false
		if len(formulae) > 0 {
			if err := remover.Run(formulae, o); err != nil {
				return err
			}
		}
		var errs []error
		if len(casks) > 0 {
			errs = append(errs, uninstallCasks(e, root, prefix, names, casks, installedCasks, dryRun, o.Force, o.IgnoreDependencies))
		}
		if autoremove {
			errs = append(errs, remover.Autoremove())
		}
		return errors.Join(append(errs, notInstalled...)...)
	})
}

func autoremoveCommand(e *env, args []string) error {
	fs := newFlagSet("autoremove")
	var dryRun bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("usage: brw autoremove [--dry-run]")
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return err
	}
	return withRoot(prefix, *journal, func(root *fsx.Root) error {
		return newRemover(e, root, prefix, kegs, dryRun).Autoremove()
	})
}

// newRemover wires the formula listing in. The listing only helps with aliases,
// config file hints and build dependencies, so it is loaded when needed and
// removing things works without it.
func newRemover(e *env, root *fsx.Root, prefix string, kegs []cellar.Keg, dryRun bool) *uninstall.Remover {
	index := sync.OnceValues(func() (*api.Index, error) { return e.index(context.Background()) })
	return &uninstall.Remover{
		Root: root, Prefix: prefix, Linker: &link.Linker{Root: root, Prefix: prefix}, Kegs: kegs,
		Resolve: func(name string) (string, bool) {
			idx, err := index()
			if err != nil {
				return name, true // no listing: report the missing keg
			}
			f, ok := idx.Lookup(name)
			if !ok {
				return name, false
			}
			return f.Name, true
		},
		FormulaNames: func() map[string]bool {
			idx, err := index()
			if err != nil {
				return nil
			}
			names := map[string]bool{}
			for _, f := range idx.Formulae() {
				names[f.Name] = true
			}
			return names
		},
		BuildDeps: func(name string) []string {
			idx, err := index()
			if err != nil {
				return nil
			}
			if f, ok := idx.Lookup(name); ok {
				return f.BuildDependencies
			}
			return nil
		},
		Out: e.out, Warn: e.err, DryRun: dryRun,
	}
}
