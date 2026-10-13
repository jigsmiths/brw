package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/install"
)

func reinstallCommand(e *env, args []string) error {
	fs := newFlagSet("reinstall")
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be reinstalled")
	fs.BoolVar(&dryRun, "n", false, "show what would be reinstalled")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw reinstall [--dry-run] FORMULA [FORMULA ...]")
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	idx, client, err := e.indexAndClient(context.Background())
	if err != nil {
		return err
	}
	kegs, err := cellar.Prefix(prefix).Kegs()
	if err != nil {
		return err
	}
	var formulae []string
	var problems []error
	for _, n := range names {
		f, ok := idx.Lookup(strings.TrimPrefix(strings.ToLower(n), "homebrew/core/"))
		switch {
		case !ok:
			problems = append(problems, fmt.Errorf("no available formula with the name %q", n))
		case !slices.Contains(formulae, f.Name):
			formulae = append(formulae, f.Name)
		}
	}
	if len(problems) > 0 {
		return errors.Join(problems...)
	}
	planner := &install.Planner{
		Index: idx, Host: e.host, Prefix: prefix, Bottles: client, Kegs: kegs,
		Linked: func(name string) bool {
			fi, err := os.Stat(filepath.Join(prefix, "var/homebrew/linked", name))
			return err == nil && fi.IsDir()
		},
	}
	plan, planErrs := planner.PlanReinstall(formulae)
	return applyPlan(e, prefix, *journal, filepath.Join(client.CacheDir, "downloads"), dryRun, "reinstalled", plan, planErrs, nil)
}
