package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/tap"
	"github.com/jigsmiths/brw/internal/trust"
	"github.com/jigsmiths/brw/internal/uninstall"
)

func untapCommand(e *env, args []string) error {
	fs := newFlagSet("untap")
	var dryRun, force bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
	fs.BoolVar(&force, "force", false, "uninstall the tap's formulae and casks without asking")
	fs.BoolVar(&force, "f", false, "uninstall the tap's formulae and casks without asking")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw untap [--force] USER/REPO [USER/REPO ...]")
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	var taps []tap.Tap
	for _, n := range names {
		name, err := tap.ParseName(n)
		if err != nil {
			return err
		}
		t := tap.Tap{Name: name, Dir: tap.Dir(prefix, name)}
		if _, err := os.Stat(t.Dir); err != nil {
			msg := "No available tap " + name + "."
			if name == tap.CaskName {
				msg += "\nRun `brw tap --force " + name + "` to tap " + name + "!"
			}
			return errors.New(msg)
		}
		if !slices.ContainsFunc(taps, func(o tap.Tap) bool { return o.Name == name }) {
			taps = append(taps, t)
		}
	}
	store, err := trust.Load(trust.Path())
	if err != nil {
		return err
	}
	return withChangeRoot(e, prefix, *journal, dryRun, func(root *fsx.Root) error {
		if !dryRun {
			lk, err := lock.Acquire(root, tapsLock)
			if err != nil {
				return err
			}
			defer func() { _ = lk.Release() }()
		}
		u := untapper{e: e, root: root, store: store, prefix: prefix, force: force, dryRun: dryRun}
		var failed []error
		for _, t := range taps {
			if err := u.untap(t); err != nil {
				failed = append(failed, err)
			}
		}
		return errors.Join(failed...)
	})
}

type untapper struct {
	e      *env
	root   *fsx.Root
	store  *trust.Store
	prefix string
	force  bool
	dryRun bool
}

type tapPackages struct {
	formulae []string
	casks    []caskroom.Cask
	names    []string
}

func (i tapPackages) empty() bool { return len(i.names) == 0 }

func (i tapPackages) types() string {
	switch {
	case len(i.formulae) == 0:
		return "casks"
	case len(i.casks) == 0:
		return "formulae"
	}
	return "formulae and casks"
}

func (u untapper) untap(t tap.Tap) error {
	if t.Name == tap.CoreName {
		return errCoreTap
	}
	var in tapPackages
	if t.Name != tap.CaskName {
		var err error
		if in, err = installedFrom(u.e, u.prefix, t.Name); err != nil {
			return err
		}
	}
	if !in.empty() {
		if err := u.uninstallPackages(t, in); err != nil {
			return err
		}
	}
	return u.removeTap(t)
}

func (u untapper) uninstallPackages(t tap.Tap, in tapPackages) error {
	list := strings.Join(in.names, "\n")
	if os.Getenv("HOMEBREW_DEVELOPER") != "" && !u.force {
		_, _ = fmt.Fprintf(u.e.err, "brw: warning: Untapping %s even though it contains the following installed %s:\n%s\n", t.Name, in.types(), list)
		return nil
	}
	if !u.force {
		if _, err := fmt.Fprintf(u.e.out, "==> Would untap %s after uninstalling the following %s:\n%s\n", t.Name, in.types(), list); err != nil {
			return err
		}
		if !u.dryRun && !u.e.ask("changes") {
			return fmt.Errorf("Refusing to untap %s because it contains the following installed %s:\n%s", t.Name, in.types(), list) //nolint:staticcheck // brew's wording
		}
	}
	kegs, err := u.e.kegs(u.prefix)
	if err != nil {
		return err
	}
	named := slices.Clone(in.formulae)
	for _, c := range in.casks {
		named = append(named, c.Token)
	}
	if len(in.formulae) > 0 {
		remover := newRemover(u.e, u.root, u.prefix, kegs, u.dryRun)
		if err := remover.Run(in.formulae, uninstall.Options{Force: u.force}); err != nil {
			return err
		}
	}
	if len(in.casks) > 0 {
		all, err := caskroom.Installed(u.prefix)
		if err != nil {
			return err
		}
		if err := uninstallCasks(u.e, u.root, u.prefix, named, in.casks, all, u.dryRun, u.force, false); err != nil {
			return err
		}
	}
	if u.dryRun {
		return nil
	}
	left, err := installedFrom(u.e, u.prefix, t.Name)
	if err != nil {
		return err
	}
	if !left.empty() {
		return fmt.Errorf("Failed to fully uninstall %s from %s", in.types(), t.Name) //nolint:staticcheck // brew's wording
	}
	return nil
}

func (u untapper) removeTap(t tap.Tap) error {
	_, _ = fmt.Fprintf(u.e.err, "Untapping %s...\n", t.Name)
	contents, size := contentsOf(t), abv(t)
	remote := t.Remote()
	entries := trustEntries(u.store, t.Name, remote)
	if !u.dryRun {
		if err := tap.ForgetDescriptions(tap.CacheDir(), t); err != nil {
			return err
		}
	}
	if err := tap.UnlinkManpages(u.root, u.prefix, t); err != nil {
		return err
	}
	if err := tap.Remove(u.root, t); err != nil {
		return err
	}
	for _, en := range entries {
		if u.dryRun {
			if _, err := fmt.Fprintf(u.e.out, "would untrust %s\n", en.name); err != nil {
				return err
			}
			continue
		}
		if _, err := u.store.Untrust(en.kind, en.name); err != nil {
			return err
		}
	}
	if !u.dryRun {
		_, _ = fmt.Fprintf(u.e.err, "Untapped%s (%s).\n", contents, size)
	}
	return nil
}

func installedFrom(e *env, prefix, tapName string) (tapPackages, error) {
	var in tapPackages
	kegs, err := e.kegs(prefix)
	if err != nil {
		return in, err
	}
	for _, k := range kegs {
		if k.Receipt != nil && strings.EqualFold(k.Receipt.Source.Tap, tapName) && !slices.Contains(in.formulae, k.Name) {
			in.formulae = append(in.formulae, k.Name)
		}
	}
	casks, err := caskroom.Installed(prefix)
	if err != nil {
		return in, err
	}
	for _, c := range casks {
		if strings.EqualFold(c.Tap, tapName) {
			in.casks = append(in.casks, c)
		}
	}
	for _, f := range in.formulae {
		in.names = append(in.names, tapName+"/"+f)
	}
	for _, c := range in.casks {
		in.names = append(in.names, tapName+"/"+c.Token)
	}
	return in, nil
}

type trustEntry struct {
	kind trust.Kind
	name string
}

func trustEntries(store *trust.Store, name, remote string) []trustEntry {
	ref := trust.Reference(name, remote)
	var out []trustEntry
	for _, en := range store.Entries(trust.KindTap) {
		if en == ref {
			out = append(out, trustEntry{trust.KindTap, en})
		}
	}
	prefixes := []string{ref + "/", strings.ToLower(name) + "/"}
	for _, kind := range []trust.Kind{trust.KindFormula, trust.KindCask} {
		for _, en := range store.Entries(kind) {
			if slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(en, p) }) {
				out = append(out, trustEntry{kind, en})
			}
		}
	}
	return out
}
