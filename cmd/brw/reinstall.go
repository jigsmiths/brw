package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

func reinstallCommand(e *env, args []string) error {
	fs := newFlagSet("reinstall")
	var dryRun bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
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
	plan, planErrs := planner.PlanReinstall(formulae)
	return applyPlan(e, prefix, *journal, filepath.Join(client.CacheDir, "downloads"), dryRun, "reinstalled", plan, planErrs, nil, withBuilds(e, prefix, idx, client))
}
