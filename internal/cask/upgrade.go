package cask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/fsx"
)

const lsregister = "/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister"

// Upgrade replaces the installed version of a cask with c, as brew's upgrade
// does: the old version's artifacts go back into its Caskroom folder, which is
// set aside until c is in and put back if c fails. It refuses, before changing
// anything, what brw cannot undo or install.
func (in *Installer) Upgrade(ctx context.Context, old caskroom.Cask, c api.Cask) error {
	u, err := in.PrepareUpgrade(ctx, old, c)
	if err != nil {
		return err
	}
	return u.Run(ctx)
}

// PreparedUpgrade is an upgrade that has been checked and downloaded.
type PreparedUpgrade struct {
	in      *Installer
	old     caskroom.Cask
	c       api.Cask
	steps   []uninstallStep
	oldApps []string
	p       installPlan
	links   []linkAction
	file    string
}

// PrepareUpgrade checks, without changing anything, that the old version can
// be replaced with c, and downloads c.
func (in *Installer) PrepareUpgrade(ctx context.Context, old caskroom.Cask, c api.Cask) (*PreparedUpgrade, error) {
	u := &PreparedUpgrade{in: in, old: old, c: c}
	var err error
	if u.steps, err = in.readUninstall(old.Token, old.Version); err != nil {
		return nil, err
	}
	owned := map[string]bool{}
	for _, s := range u.steps {
		switch s.kind {
		case "app":
			u.oldApps = append(u.oldApps, s.move.target)
			owned[s.move.target] = true
		case "binary":
			owned[s.move.target] = true
		}
	}
	version := filepath.Join("Caskroom", c.Token, c.Version)
	if _, err := in.Root.Lstat(version); err == nil && c.Version != old.Version {
		return nil, fmt.Errorf("%s: %s: %w", c.Token, version, fsx.ErrExists)
	}
	if u.p, u.links, u.file, err = in.prepare(ctx, c, owned); err != nil {
		return nil, err
	}
	return u, nil
}

// Run carries out the upgrade.
func (u *PreparedUpgrade) Run(ctx context.Context) error {
	in, old, c := u.in, u.old, u.c
	in.logf("==> Upgrading %s", c.Token)
	in.logf("  %s -> %s", old.Version, c.Version)
	before := in.approvals(ctx, u.oldApps)
	in.upgrading, in.reopen = true, nil
	defer func() { in.upgrading, in.reopen = false, nil }()
	// Like brew, a failure here leaves what was already removed: nothing was
	// set aside yet to put back.
	if err := in.removeArtifacts(ctx, old.Token, u.steps, false); err != nil {
		return err
	}
	if err := in.backup(old); err != nil {
		return err
	}
	if err := in.replace(ctx, old, c, u.p, u.links, u.file, u.steps, before); err != nil {
		return err
	}
	return in.writeReceipt(c, u.p, old.OnRequest)
}

// replace installs c in place of the old version, already set aside, and
// puts the old version back if that fails.
func (in *Installer) replace(ctx context.Context, old caskroom.Cask, c api.Cask, p installPlan, links []linkAction, file string, steps []uninstallStep, before []approval) (err error) {
	var undo []func() error
	defer func() {
		if err == nil {
			return
		}
		err = errors.Join(err, in.discard(ctx, c, undo))
		in.warnf("Reverting upgrade for Cask %s", c.Token)
		err = errors.Join(err, in.restoreBackup(old), in.reinstall(context.WithoutCancel(ctx), old, steps))
	}()
	if err := in.installArtifacts(ctx, c, p, links, file, &undo); err != nil {
		return err
	}
	in.releaseQuarantine(ctx, c.Token, before, p.apps)
	in.logf("==> Purging files for version %s of Cask %s", old.Version, c.Token)
	if err := in.purgeBackup(ctx, old); err != nil {
		return err
	}
	in.logf("%s was successfully upgraded!", badge()+c.Token)
	in.reopenApps(ctx, p.apps)
	return nil
}

// backupPaths are where brew sets an old version's Caskroom folder and
// metadata aside during an upgrade.
func backupPaths(old caskroom.Cask) (staged, meta string) {
	dir := filepath.Join("Caskroom", old.Token)
	return filepath.Join(dir, old.Version), filepath.Join(dir, ".metadata", old.Version)
}

const upgrading = ".upgrading"

// backup is brew's: either folder may already be gone.
func (in *Installer) backup(old caskroom.Cask) error {
	staged, meta := backupPaths(old)
	for _, p := range []string{staged, meta} {
		if _, err := in.Root.Lstat(p); err != nil {
			continue
		}
		if err := in.Root.Rename(p, p+upgrading); err != nil {
			return err
		}
	}
	return nil
}

// restoreBackup is brew's: it puts back only a complete backup.
func (in *Installer) restoreBackup(old caskroom.Cask) error {
	staged, meta := backupPaths(old)
	for _, p := range []string{staged, meta} {
		if fi, err := in.Root.Lstat(p + upgrading); err != nil || !fi.IsDir() {
			return nil
		}
	}
	for _, p := range []string{staged, meta} {
		if err := removeIfThere(in.Root, p); err != nil {
			return err
		}
		if err := in.Root.Rename(p+upgrading, p); err != nil {
			return err
		}
	}
	return nil
}

// purgeBackup is brew's purge_backed_up_versioned_files.
func (in *Installer) purgeBackup(ctx context.Context, old caskroom.Cask) error {
	staged, meta := backupPaths(old)
	if _, err := in.Root.Lstat(staged + upgrading); err == nil {
		if err := in.removePath(ctx, in.Root, staged+upgrading); err != nil {
			return err
		}
	}
	entries, err := in.Root.ReadDir(meta + upgrading)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if err := in.removePath(ctx, in.Root, filepath.Join(meta+upgrading, e.Name())); err != nil {
			return err
		}
	}
	_ = in.Root.Remove(meta + upgrading)
	return nil
}

// reinstall puts the old version's artifacts back from its Caskroom folder,
// as brew's revert_upgrade.
func (in *Installer) reinstall(ctx context.Context, old caskroom.Cask, steps []uninstallStep) error {
	version := filepath.Join("Caskroom", old.Token, old.Version)
	staged, err := in.Root.Abs(version)
	if err != nil {
		return err
	}
	oc := api.Cask{Token: old.Token, Version: old.Version}
	var errs error
	var undo []func() error // nothing to undo: this is the last resort
	for _, s := range steps {
		switch s.kind {
		case "app":
			rel, err := filepath.Rel(version, s.move.source)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			errs = errors.Join(errs, in.moveApp(ctx, version, move{source: rel, target: s.move.target}, false, &undo))
		case "binary":
			action, _, err := in.binaryAction(oc, s.move)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			if action == alreadyLinked || action == skipLink {
				continue
			}
			errs = errors.Join(errs, in.linkBinary(ctx, staged, s.move, action, &undo))
		case "generate_completions_from_executable":
			errs = errors.Join(errs, in.generateCompletions(ctx, old.Token, staged, s.generator, &undo))
		case "preflight_steps", "postflight_steps":
			errs = errors.Join(errs, in.runSteps(s.kind, old.Token, old.Version, staged, s.steps))
		}
	}
	return errs
}

// reopenApps opens again the apps quit for the upgrade, as brew does, after
// registering the new apps with Launch Services.
func (in *Installer) reopenApps(ctx context.Context, apps []move) {
	if len(in.reopen) == 0 {
		return
	}
	if fi, err := os.Stat(lsregister); err == nil && fi.Mode()&0o111 != 0 {
		for _, m := range apps {
			if _, err := os.Stat(m.target); err == nil {
				_, _ = in.exec(ctx, lsregister, "-f", m.target)
			}
		}
	}
	noun := "applications"
	if len(in.reopen) == 1 {
		noun = "application"
	}
	in.logf("==> Reopening %d %s closed during upgrade:", len(in.reopen), noun)
	for _, id := range in.reopen {
		in.logf("%s", id)
		// brew clears the environment so Launch Services supplies the user's.
		_, _ = in.exec(ctx, "/usr/bin/env", "-i", "/usr/bin/open", "-b", id)
	}
}
