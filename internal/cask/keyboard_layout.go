package cask

import (
	"context"
	"os"
	"path/filepath"

	"github.com/jigsmiths/brw/internal/sudo"
)

const (
	keyboardLayoutName = "Keyboard Layout"
	intlDataCache      = "/System/Library/Caches/com.apple.IntlDataCache.le*"
)

func occupied(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func (in *Installer) moveKeyboardLayout(ctx context.Context, version string, m move, undo *[]func() error) error {
	staged := filepath.Join(version, m.source)
	abs, err := in.Root.Abs(staged)
	if err != nil {
		return err
	}
	if _, err := in.Root.Lstat(staged); err != nil {
		return brewError("It seems the %s source '%s' is not there.", keyboardLayoutName, abs)
	}
	if occupied(m.target) {
		if !in.Adopt {
			return brewError("It seems there is already a %s at '%s'.", keyboardLayoutName, m.target)
		}
		in.logf("==> Adopting existing %s at '%s'", keyboardLayoutName, m.target)
		same, err := in.sameApp(ctx, abs, m.target)
		if err != nil {
			return err
		}
		if !same {
			return brewError("It seems the existing %s is different from the one being installed.", keyboardLayoutName)
		}
		if err := in.Root.RemoveAll(staged); err != nil {
			return err
		}
		return in.linkKeyboardLayout(ctx, staged, m, undo)
	}
	in.logf("==> Moving %s '%s' to '%s'", keyboardLayoutName, filepath.Base(m.source), m.target)
	if dir := filepath.Dir(m.target); !occupied(dir) {
		if err := in.sudoIfNeeded(ctx, "/bin/mkdir", "-p", "--", dir); err != nil {
			return err
		}
	}
	if err := in.sudoIfNeeded(ctx, "/bin/cp", "-pR", abs, m.target); err != nil {
		return err
	}
	*undo = append(*undo, func() error { return in.gainPermissionsRemove(context.WithoutCancel(ctx), m.target) })
	if err := in.removePath(ctx, in.Root, staged); err != nil {
		return err
	}
	return in.linkKeyboardLayout(ctx, staged, m, undo)
}

func (in *Installer) linkKeyboardLayout(ctx context.Context, staged string, m move, undo *[]func() error) error {
	if err := in.Root.SymlinkAny(m.target, staged); err != nil {
		return err
	}
	*undo = append(*undo, func() error { return in.Root.Remove(staged) })
	return in.deleteKeyboardLayoutCache(ctx)
}

func (in *Installer) deleteKeyboardLayoutCache(ctx context.Context) error {
	_, err := in.capture(ctx, in.command(sudo.Root, "/bin/rm", "-f", "--", intlDataCache))
	return err
}

func (in *Installer) moveBackKeyboardLayout(ctx context.Context, m move, force bool) error {
	staged := m.source
	if dest, err := in.Root.Readlink(staged); err == nil && dest == m.target {
		if err := in.Root.Remove(staged); err != nil {
			return err
		}
	}
	abs, err := in.Root.Abs(staged)
	if err != nil {
		return err
	}
	if _, err := in.Root.Lstat(staged); err == nil {
		if !force {
			return brewError("It seems there is already a %s at '%s'.", keyboardLayoutName, abs)
		}
		in.warnf("It seems there is already a %s at '%s'; overwriting.", keyboardLayoutName, abs)
		in.logf("==> Removing %s '%s'", keyboardLayoutName, abs)
		if err := in.Root.RemoveAll(staged); err != nil {
			return err
		}
	}
	if _, err := os.Stat(m.target); err != nil {
		if force {
			return in.deleteKeyboardLayoutCache(ctx)
		}
		return brewError("It seems the %s source '%s' is not there.", keyboardLayoutName, m.target)
	}
	in.logf("==> Backing up %s '%s' to '%s'", keyboardLayoutName, filepath.Base(m.target), abs)
	if err := in.Root.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		return err
	}
	if err := in.sudoIfNeeded(ctx, "/bin/cp", in.backupArgs(m.target, abs)...); err != nil {
		return err
	}
	in.logf("==> Removing %s '%s'", keyboardLayoutName, m.target)
	if undeletable(m.target, in.Where.Home) {
		return brewError("Cannot remove undeletable %s.", keyboardLayoutName)
	}
	if err := in.gainPermissionsRemove(ctx, m.target); err != nil {
		return err
	}
	return in.deleteKeyboardLayoutCache(ctx)
}
