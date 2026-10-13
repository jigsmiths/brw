package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

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
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be upgraded")
	fs.BoolVar(&dryRun, "n", false, "show what would be upgraded")
	names, err := parse(fs, args)
	if err != nil {
		return err
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
	stale, err := outdated.Check(idx, cellar.Prefix(prefix), kegs)
	if err != nil {
		return err
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
			return errors.Join(problems...)
		}
	}
	var failures []error
	if len(pinned) > 0 {
		msg := fmt.Sprintf("not upgrading %d pinned %s:", len(pinned), map[bool]string{true: "package", false: "packages"}[len(pinned) == 1])
		for _, r := range pinned {
			msg += fmt.Sprintf("\n%s %s", r.Name, r.PinnedVersion)
		}
		failures = append(failures, errors.New(msg))
	}
	if len(upgrade) == 0 {
		return errors.Join(failures...)
	}

	planner := &install.Planner{
		Index: idx, Host: e.host, Prefix: prefix, Bottles: client, Kegs: kegs,
		Linked: func(name string) bool {
			fi, err := os.Stat(filepath.Join(prefix, "var/homebrew/linked", name))
			return err == nil && fi.IsDir()
		},
	}
	upgradeNames := make([]string, len(upgrade))
	for i, r := range upgrade {
		upgradeNames[i] = r.Name
	}
	plan, planErrs := planner.PlanUpgrade(upgradeNames)
	for _, err := range planErrs {
		_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
	}
	if len(planErrs) > 0 {
		failures = append(failures, fmt.Errorf("%d formulae cannot be upgraded", len(planErrs)))
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
		return errors.Join(append(failures, printUpgradePlan(e, prefix, plan))...)
	}
	err = withRoot(prefix, *journal, func(root *fsx.Root) error {
		return runInstall(e, root, prefix, filepath.Join(client.CacheDir, "downloads"), plan, func(in *install.Installer) {
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

// printUpgradePlan says what an upgrade would do.
func printUpgradePlan(e *env, prefix string, plan *install.Plan) error {
	var upgrades, installs []string
	for _, it := range plan.Items {
		if len(it.Upgrade) > 0 {
			old := cellar.Prefix(prefix).DefaultKeg(it.Upgrade)
			upgrades = append(upgrades, fmt.Sprintf("%s %s -> %s", it.Formula.Name, old.Version, it.PkgVersion))
		} else {
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
	kegs, err := cellar.Prefix(prefix).Kegs()
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
