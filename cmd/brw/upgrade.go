package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/cleanup"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/install"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/racks"
	"github.com/jigsmiths/brw/internal/version"
)

func upgradeCommand(e *env, args []string) error {
	fs := newFlagSet("upgrade")
	var dryRun bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
	kinds := kindFlags(fs)
	var o outdated.CaskOptions
	fs.BoolVar(&o.Greedy, "greedy", false, "include casks that update themselves or are unversioned")
	fs.BoolVar(&o.GreedyLatest, "greedy-latest", false, "include unversioned casks")
	fs.BoolVar(&o.GreedyAutoUpdates, "greedy-auto-updates", false, "include casks that update themselves")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	formulaOnly, caskOnly, err := kinds()
	if err != nil {
		return err
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	formulae, casks := names, []string(nil)
	if len(names) > 0 && !formulaOnly {
		if formulae, casks, err = e.splitUpgradeNames(prefix, names, caskOnly); err != nil {
			return err
		}
	}
	var failures []error
	found := false
	if !caskOnly && (len(names) == 0 || len(formulae) > 0) {
		f, err := upgradeFormulae(e, prefix, *journal, dryRun, formulae)
		found = found || f
		failures = append(failures, err)
	}
	if !formulaOnly && (len(names) == 0 || len(casks) > 0) {
		f, err := upgradeCasks(e, prefix, *journal, dryRun, casks, o)
		found = found || f
		failures = append(failures, err)
	}
	if !found && len(names) == 0 {
		_, _ = fmt.Fprintln(e.out, "Nothing to upgrade.")
	}
	return errors.Join(failures...)
}

// splitUpgradeNames sorts the names into formulae and casks: a name is a
// formula when the listing or the Cellar has one, else a cask when the cask
// listing has one. A name that is neither goes with the formulae, which say
// what is wrong with it.
func (e *env) splitUpgradeNames(prefix string, names []string, caskOnly bool) (formulae, casks []string, err error) {
	if caskOnly {
		return nil, names, nil
	}
	idx, err := e.index(context.Background())
	if err != nil {
		return nil, nil, err
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return nil, nil, err
	}
	finder := racks.Finder{Prefix: prefix, Kegs: kegs, Resolve: func(name string) (string, bool) {
		f, ok := idx.Lookup(name)
		if !ok {
			return name, false
		}
		return f.Name, true
	}}
	var caskIdx *api.CaskIndex
	for _, n := range names {
		if _, ok := idx.Lookup(n); ok {
			formulae = append(formulae, n)
			continue
		}
		if _, err := finder.Find(n); err == nil {
			formulae = append(formulae, n)
			continue
		}
		if caskIdx == nil {
			if caskIdx, err = e.caskIndex(context.Background(), prefix); err != nil {
				return nil, nil, err
			}
		}
		if _, ok := caskIdx.Lookup(n); ok {
			casks = append(casks, n)
			continue
		}
		formulae = append(formulae, n)
	}
	return formulae, casks, nil
}

// upgradeFormulae upgrades the named formulae, or every outdated one. found
// says whether any was out of date.
func upgradeFormulae(e *env, prefix, journal string, dryRun bool, names []string) (found bool, err error) {
	idx, client, err := e.indexAndClient(context.Background())
	if err != nil {
		return false, err
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return false, err
	}
	stale, err := outdated.Check(idx, cellar.Prefix(prefix), kegs)
	if err != nil {
		return false, err
	}

	var upgrade, pinned []outdated.Result
	if len(names) == 0 {
		for _, r := range stale {
			if r.Pinned {
				continue // brew leaves pinned formulae alone without a word
			}
			upgrade = append(upgrade, r)
		}
	} else {
		finder := racks.Finder{Prefix: prefix, Kegs: kegs, Resolve: func(name string) (string, bool) {
			f, ok := idx.Lookup(name)
			if !ok {
				return name, false
			}
			return f.Name, true
		}}
		var problems []error
		for _, n := range names {
			rack, err := finder.Find(n)
			if err != nil {
				problems = append(problems, err)
				continue
			}
			i := slices.IndexFunc(stale, func(r outdated.Result) bool { return r.Name == rack })
			switch {
			case i < 0:
				f, _ := idx.Lookup(rack)
				_, _ = fmt.Fprintf(e.err, "brw: warning: %s %s already installed\n", rack, pkgVersionOf(f.Versions.Stable, f.Revision))
			case stale[i].Pinned:
				pinned = append(pinned, stale[i])
			default:
				upgrade = append(upgrade, stale[i])
			}
		}
		if len(problems) > 0 {
			return false, errors.Join(problems...)
		}
	}
	var failures []error
	if len(pinned) > 0 {
		var msg strings.Builder
		fmt.Fprintf(&msg, "not upgrading %d pinned %s:", len(pinned), plural(len(pinned), "package", "packages"))
		for _, r := range pinned {
			fmt.Fprintf(&msg, "\n%s %s", r.Name, r.PinnedVersion)
		}
		failures = append(failures, errors.New(msg.String()))
	}
	if len(upgrade) == 0 {
		return false, errors.Join(failures...)
	}

	planner := e.newPlanner(prefix, idx, client, kegs)
	upgradeNames := make([]string, len(upgrade))
	for i, r := range upgrade {
		upgradeNames[i] = r.Name
	}
	plan, planErrs := planner.PlanUpgrade(upgradeNames)
	return true, applyPlan(e, prefix, journal, filepath.Join(client.CacheDir, "downloads"), dryRun, "upgraded", plan, planErrs, failures)
}

// applyPlan reports what could not be planned, then shows the plan or carries it
// out, cleaning up each formula's old kegs after it. The failures passed in are
// returned with any new ones. what ends the "cannot be ..." message, for example
// "upgraded".
func applyPlan(e *env, prefix, journal, downloads string, dryRun bool, what string, plan *install.Plan, planErrs, failures []error) error {
	for _, err := range planErrs {
		_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
	}
	if len(planErrs) > 0 {
		failures = append(failures, fmt.Errorf("%d formulae cannot be %s", len(planErrs), what))
	}
	for _, it := range plan.Items {
		for _, w := range it.Warnings {
			_, _ = fmt.Fprintln(e.err, "brw: warning:", w)
		}
	}
	if len(plan.Items) == 0 {
		return errors.Join(failures...)
	}
	if dryRun {
		return errors.Join(append(failures, printPlan(e, prefix, plan))...)
	}
	err := withRoot(prefix, journal, func(root *fsx.Root) error {
		return runInstall(e, root, prefix, downloads, plan, func(in *install.Installer) {
			in.KeepGoing = true
			in.After = func(it install.Item, _ install.Result) { cleanAfterUpgrade(e, root, prefix, it) }
		})
	})
	return errors.Join(append(failures, err)...)
}

func pkgVersionOf(stable string, revision int) string {
	v, err := version.NewPkg(stable, revision)
	if err != nil {
		return stable
	}
	return v.String()
}

// printPlan says what an upgrade or reinstall would do.
func printPlan(e *env, prefix string, plan *install.Plan) error {
	var upgrades, reinstalls, installs []string
	for _, it := range plan.Items {
		switch {
		case it.Reinstall:
			reinstalls = append(reinstalls, fmt.Sprintf("%s %s", it.Formula.Name, it.PkgVersion))
		case len(it.Upgrade) > 0:
			old := cellar.Prefix(prefix).DefaultKeg(it.Upgrade)
			upgrades = append(upgrades, fmt.Sprintf("%s %s -> %s", it.Formula.Name, old.Version, it.PkgVersion))
		default:
			installs = append(installs, fmt.Sprintf("%s %s", it.Formula.Name, it.PkgVersion))
		}
	}
	var b bytes.Buffer
	section := func(verb string, lines []string) {
		if len(lines) == 0 {
			return
		}
		noun := "formulae"
		if len(lines) == 1 {
			noun = "formula"
		}
		fmt.Fprintf(&b, "==> Would %s %d %s:\n", verb, len(lines), noun)
		for _, l := range lines {
			fmt.Fprintln(&b, l)
		}
	}
	section("upgrade", upgrades)
	section("reinstall", reinstalls)
	section("install", installs)
	_, err := e.out.Write(b.Bytes())
	return err
}

// cleanAfterUpgrade removes the old kegs of a formula that was just upgraded,
// unless HOMEBREW_NO_INSTALL_CLEANUP asks for them to be kept.
func cleanAfterUpgrade(e *env, root *fsx.Root, prefix string, it install.Item) {
	if len(it.Upgrade) == 0 || os.Getenv("HOMEBREW_NO_INSTALL_CLEANUP") != "" || skipCleanup()(it.Formula.Name) {
		return
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return
	}
	var out bytes.Buffer
	c := &cleanup.Cleaner{
		Root: root, Prefix: prefix, Linker: &link.Linker{Root: root, Prefix: prefix}, Kegs: kegs,
		Current: func(name string) (cleanup.Formula, bool) {
			if name != it.Formula.Name {
				return cleanup.Formula{}, false
			}
			return cleanup.Formula{PkgVersion: it.PkgVersion, Scheme: it.Formula.VersionScheme, Rebuild: it.Formula.Bottle.Stable.Rebuild}, true
		},
		Now: time.Now, Out: &out, Warn: e.err, LocksHeld: true, // the installer still holds them
	}
	if err := c.OldKegs([]string{it.Formula.Name}); err != nil {
		_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
	}
	if out.Len() > 0 {
		_, _ = fmt.Fprintf(e.out, "==> Running `brw cleanup %s`...\n%s", it.Formula.Name, out.String())
	}
}
