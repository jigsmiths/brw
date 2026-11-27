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

// PreparedUpgrade sets the old version's Caskroom folder aside until the new one is in, and restores it on failure.
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
		case "binary", "manpage", "bash_completion", "zsh_completion", "fish_completion":
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

func (u *PreparedUpgrade) Run(ctx context.Context) error {
	in, old, c := u.in, u.old, u.c
	in.logf("==> Upgrading %s", c.Token)
	in.logf("  %s -> %s", old.Version, c.Version)
	if err := in.satisfyDependencies(ctx, c); err != nil {
		return err
	}
	before := in.approvals(ctx, u.oldApps)
	in.upgrading, in.reopen = true, nil
	defer func() { in.upgrading, in.reopen = false, nil }()
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

func (in *Installer) replace(ctx context.Context, old caskroom.Cask, c api.Cask, p installPlan, links []linkAction, file string, steps []uninstallStep, before []approval) (err error) {
	var undo []func() error
	defer func() {
		if err == nil {
			return
		}
		err = errors.Join(err, in.discard(ctx, c, undo))
		in.warnf("Reverting upgrade for Cask %s", c.Token)
		if rerr := errors.Join(in.restoreBackup(old), in.reinstall(context.WithoutCancel(ctx), old, steps)); rerr != nil {
			err = errors.Join(err, fmt.Errorf("could not restore %s %s: %w", c.Token, old.Version, rerr))
		}
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

func backupPaths(old caskroom.Cask) (staged, meta string) {
	dir := filepath.Join("Caskroom", old.Token)
	return filepath.Join(dir, old.Version), filepath.Join(dir, ".metadata", old.Version)
}

const upgrading = ".upgrading"

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

func (in *Installer) reinstall(ctx context.Context, old caskroom.Cask, steps []uninstallStep) error {
	version := filepath.Join("Caskroom", old.Token, old.Version)
	staged, err := in.Root.Abs(version)
	if err != nil {
		return err
	}
	oc := api.Cask{Token: old.Token, Version: old.Version}
	var errs error
	var undo []func() error
	for _, s := range steps {
		switch s.kind {
		case "app":
			rel, err := filepath.Rel(version, s.move.source)
			if err != nil {
				errs = errors.Join(errs, err)
				continue
			}
			errs = errors.Join(errs, in.moveApp(ctx, version, move{source: rel, target: s.move.target}, false, &undo))
		case "binary", "manpage", "bash_completion", "zsh_completion", "fish_completion":
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
			errs = errors.Join(errs, in.runSteps(ctx, s.kind, old.Token, old.Version, staged, s.steps))
		}
	}
	return errs
}

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
		_, _ = in.exec(ctx, "/usr/bin/env", "-i", "/usr/bin/open", "-b", id)
	}
}
