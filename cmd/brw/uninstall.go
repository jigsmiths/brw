package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/uninstall"
)

func uninstallCommand(e *env, args []string) error {
	fs := newFlagSet("uninstall")
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	var o uninstall.Options
	var dryRun bool
	fs.BoolVar(&o.Force, "force", false, "remove every installed version")
	fs.BoolVar(&o.Force, "f", false, "remove every installed version")
	fs.BoolVar(&o.IgnoreDependencies, "ignore-dependencies", false, "remove even if installed formulae need it")
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be removed")
	fs.BoolVar(&dryRun, "n", false, "show what would be removed")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw uninstall [--force] [--ignore-dependencies] [--dry-run] FORMULA [FORMULA ...]")
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	kegs, err := cellar.Prefix(prefix).Kegs()
	if err != nil {
		return err
	}
	return withRoot(prefix, *journal, func(root *fsx.Root) error {
		o.Autoremove = os.Getenv("HOMEBREW_NO_AUTOREMOVE") == ""
		return newRemover(e, root, prefix, kegs, dryRun).Run(names, o)
	})
}

func autoremoveCommand(e *env, args []string) error {
	fs := newFlagSet("autoremove")
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be removed")
	fs.BoolVar(&dryRun, "n", false, "show what would be removed")
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
	kegs, err := cellar.Prefix(prefix).Kegs()
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
