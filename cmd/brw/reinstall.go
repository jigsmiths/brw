package main

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/outdated"
)

func reinstallCommand(e *env, args []string) error {
	fs := newFlagSet("reinstall")
	var dryRun bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
	kinds := kindFlags(fs)
	trace := traceFlags(fs)
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw reinstall [--formula|--cask] [--verbose] [--debug] [--dry-run] NAME [NAME ...]")
	}
	formulaOnly, caskOnly, err := kinds()
	if err != nil {
		return err
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	for i, n := range names {
		names[i] = strings.ToLower(n)
	}
	r, err := e.resolve(context.Background(), prefix, names, formulaOnly, caskOnly)
	if err != nil {
		return err
	}
	// Unlike brew, which reinstalls the names it knows, an unknown name stops everything.
	if len(r.failures) > 0 {
		return errors.Join(r.failures...)
	}
	var failures []error
	if len(r.formulae) > 0 {
		failures = append(failures, reinstallFormulae(e, prefix, *journal, dryRun, r.idx, r.client, r.formulae))
	}
	if len(r.casks) > 0 {
		failures = append(failures, reinstallCasks(e, prefix, *journal, dryRun, *trace, r.casks))
	}
	return errors.Join(failures...)
}

func reinstallFormulae(e *env, prefix, journal string, dryRun bool, idx *api.Index, client *api.Client, names []string) error {
	kegs, err := e.kegs(prefix)
	if err != nil {
		return err
	}
	var formulae []string
	for _, n := range names {
		if f, ok := idx.Lookup(n); ok && !slices.Contains(formulae, f.Name) {
			formulae = append(formulae, f.Name)
		}
	}
	planner := e.newPlanner(prefix, idx, client, kegs)
	for _, name := range formulae {
		var own []cellar.Keg
		for _, k := range kegs {
			if k.Name == name {
				own = append(own, k)
			}
		}
		if f, group, ok := outdated.Formula(idx, name, own); ok && f.Versions.Head != "" && cellar.Prefix(prefix).DefaultKeg(group).Receipt.IsHeadBuild() {
			planner.HeadNames = append(planner.HeadNames, name)
		}
	}
	plan, planErrs := planner.PlanReinstall(formulae)
	return applyPlan(e, prefix, journal, filepath.Join(client.CacheDir, "downloads"), dryRun, "reinstalled", plan, planErrs, nil, withBuilds(e, prefix, idx, client), nil)
}
