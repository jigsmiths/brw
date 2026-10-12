package main

import (
	"context"
	"errors"
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
	// The formula listing only helps with aliases and config file hints, so it
	// is loaded when needed, and uninstalling works without it.
	index := sync.OnceValues(func() (*api.Index, error) { return e.index(context.Background()) })
	listing := func() map[string]bool {
		idx, err := index()
		if err != nil {
			return nil
		}
		names := map[string]bool{}
		for _, f := range idx.Formulae() {
			names[f.Name] = true
		}
		return names
	}
	return withRoot(prefix, *journal, func(root *fsx.Root) error {
		return (&uninstall.Remover{
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
			FormulaNames: listing,
			Out:          e.out, Warn: e.err, DryRun: dryRun,
		}).Run(names, o)
	})
}
