package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/outdated"
)

func reinstallCommand(e *env, args []string) error {
	fs := newFlagSet("reinstall")
	var dryRun bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
	traceFlags(fs)
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw reinstall [--verbose] [--debug] [--dry-run] FORMULA [FORMULA ...]")
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	idx, client, err := e.indexAndClient(context.Background(), prefix)
	if err != nil {
		return err
	}
	kegs, err := e.kegs(prefix)
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
	return applyPlan(e, prefix, *journal, filepath.Join(client.CacheDir, "downloads"), dryRun, "reinstalled", plan, planErrs, nil, withBuilds(e, prefix, idx, client), nil)
}
