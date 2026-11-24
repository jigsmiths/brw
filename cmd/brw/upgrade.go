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
	fetchHead := fs.Bool("fetch-HEAD", false, "fetch each HEAD formula's repository and upgrade it if there is a newer commit")
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
	var steps []*upgradeStep
	if !caskOnly && (len(names) == 0 || len(formulae) > 0) {
		steps = append(steps, resolveFormulae(e, prefix, *journal, dryRun, *fetchHead, formulae))
	}
	if !formulaOnly && (len(names) == 0 || len(casks) > 0) {
		steps = append(steps, resolveCasks(e, prefix, *journal, dryRun, casks, o))
	}
	if !dryRun {
		announceUpgrades(e, steps)
	}
	var failures []error
	found := false
	var done []string
	for _, s := range steps {
		failures = append(failures, s.failures...)
		if len(s.upgrade) > 0 {
			found = true
			names, err := s.run()
			done = append(done, names...)
			failures = append(failures, err)
		}
	}
	if !dryRun {
		summarizeUpgrades(e, steps, done)
	}
	if !found && len(names) == 0 {
		_, _ = fmt.Fprintln(e.out, "Nothing to upgrade.")
	}
	return errors.Join(failures...)
}

type upgradeStep struct {
	upgrade  []outdated.Result
	pinned   []outdated.Result
	failures []error
	run      func() (upgraded []string, err error)
}

func failedStep(err error) *upgradeStep { return &upgradeStep{failures: []error{err}} }

// Unlike brew, this also lists what failed, by name, for formulae and casks alike.
func summarizeUpgrades(e *env, steps []*upgradeStep, done []string) {
	var upgraded, failed []outdated.Result
	for _, s := range steps {
		for _, r := range s.upgrade {
			if slices.Contains(done, r.Name) {
				upgraded = append(upgraded, r)
			} else {
				failed = append(failed, r)
			}
		}
	}
	// brew sums up only when more than one package changed.
	if len(upgraded) > 1 {
		_, _ = fmt.Fprintf(e.out, "==> Upgraded %d outdated packages\n%s\n", len(upgraded), strings.Join(versionChanges(upgraded), "\n"))
	}
	if len(failed) > 0 {
		_, _ = fmt.Fprintf(e.out, "==> Failed to upgrade %d %s:\n%s\n", len(failed),
			plural(len(failed), "package", "packages"), strings.Join(versionChanges(failed), "\n"))
	}
}

// Unlike brew, this also lists pinned packages and untrusted taps it leaves out.
func announceUpgrades(e *env, steps []*upgradeStep) {
	var upgrade, pinned []outdated.Result
	for _, s := range steps {
		upgrade = append(upgrade, s.upgrade...)
		pinned = append(pinned, s.pinned...)
	}
	if len(upgrade) == 0 {
		return
	}
	_, _ = fmt.Fprintf(e.out, "==> Upgrading %d outdated %s:\n%s\n", len(upgrade),
		plural(len(upgrade), "package", "packages"), strings.Join(versionChanges(upgrade), "\n"))
	if len(pinned) > 0 {
		_, _ = fmt.Fprintf(e.out, "==> Pinned, not upgrading %d %s:\n%s\n", len(pinned),
			plural(len(pinned), "package", "packages"), strings.Join(versionChanges(pinned), "\n"))
	}
	if taps := e.warned.untrustedTaps(); len(taps) > 0 {
		_, _ = fmt.Fprintf(e.out, "==> Skipping %s because %s not trusted\n", strings.Join(taps, ", "), plural(len(taps), "it is", "they are"))
	}
}

func (e *env) splitUpgradeNames(prefix string, names []string, caskOnly bool) (formulae, casks []string, err error) {
	if caskOnly {
		return nil, names, nil
	}
	idx, err := e.index(context.Background(), prefix)
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

func resolveFormulae(e *env, prefix, journal string, dryRun, fetchHead bool, names []string) *upgradeStep {
	idx, client, err := e.indexAndClient(context.Background(), prefix)
	if err != nil {
		return failedStep(err)
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return failedStep(err)
	}
	stale, err := outdated.CheckWith(idx, cellar.Prefix(prefix), kegs, e.headOptions(client, fetchHead))
	if err != nil {
		return failedStep(err)
	}

	otherTap := outdated.OtherTap(idx, kegs)
	var upgrade, pinned, left []outdated.Result
	if len(names) == 0 {
		e.warnOtherTap(idx, kegs, otherTap)
		for _, r := range stale {
			if r.Pinned {
				left = append(left, r)
				continue
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
			case slices.Contains(otherTap, rack):
				if err := tapFormulaError(idx, kegs, rack); err != nil {
					problems = append(problems, err)
				} else {
					problems = append(problems, fmt.Errorf("%s: %w", rack, install.ErrOtherTap))
				}
			case i < 0:
				f, group, _ := outdated.Formula(idx, rack, finder.Installed(rack))
				if def := cellar.Prefix(prefix).DefaultKeg(group); f.Versions.Head != "" && def.Receipt.IsHeadBuild() {
					_, _ = fmt.Fprintf(e.err, "brw: warning: %s %s already installed\n", f.FullName, def.Version)
					continue
				}
				_, _ = fmt.Fprintf(e.err, "brw: warning: %s %s already installed\n", rack, pkgVersionOf(f.Versions.Stable, f.Revision))
			case stale[i].Pinned:
				pinned = append(pinned, stale[i])
			default:
				upgrade = append(upgrade, stale[i])
			}
		}
		if len(problems) > 0 {
			return failedStep(errors.Join(problems...))
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
	return &upgradeStep{upgrade: upgrade, pinned: left, failures: failures, run: func() ([]string, error) {
		var done []string
		planner := e.newPlanner(prefix, idx, client, kegs)
		upgradeNames := make([]string, len(upgrade))
		for i, r := range upgrade {
			upgradeNames[i] = r.FullName
			if r.Head {
				planner.HeadNames = append(planner.HeadNames, r.FullName)
			}
		}
		plan, planErrs := planner.PlanUpgrade(upgradeNames)
		err := applyPlan(e, prefix, journal, filepath.Join(client.CacheDir, "downloads"), dryRun, "upgraded", plan, planErrs, nil,
			withBuilds(e, prefix, idx, client), func(name string) { done = append(done, name) })
		return done, err
	}}
}

func applyPlan(e *env, prefix, journal, downloads string, dryRun bool, what string, plan *install.Plan, planErrs, failures []error, builds func(*install.Installer), onDone func(name string)) error {
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
		return runInstall(e, root, prefix, downloads, plan, builds, func(in *install.Installer) {
			in.KeepGoing = true
			in.After = func(it install.Item, _ install.Result) {
				cleanAfterUpgrade(e, root, prefix, it)
				if onDone != nil {
					onDone(it.Formula.Name)
				}
			}
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
			return cleanup.Formula{PkgVersion: it.PkgVersion, Scheme: it.Formula.VersionScheme, Rebuild: it.Formula.Bottle.Stable.Rebuild, HasHead: it.Formula.Versions.Head != ""}, true
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
