package cask

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/sudo"
)

func (in *Installer) sudoIfNeeded(ctx context.Context, name string, args ...string) error {
	_, err := in.system(ctx, name, args...)
	if err == nil || !sudo.Available(ctx, in.runner()) {
		return err
	}
	retry := in.command(sudo.Sudo, name, args...)
	retry.Stderr = nil
	_, err = in.capture(ctx, retry)
	return err
}

func (in *Installer) removePath(ctx context.Context, r *fsx.Root, rel string) error {
	fi, err := r.Lstat(rel)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	abs, err := r.Abs(rel)
	if err != nil {
		return err
	}
	dir := fi.IsDir()
	var flags []string
	switch {
	case fi.Mode()&fs.ModeSymlink != 0:
		flags = []string{"-h"}
	case dir:
		flags = []string{"-R"}
	}
	return in.gainPermissions(ctx, abs, flags, func(p string) error {
		if writable(filepath.Dir(p)) {
			return r.RemoveAll(rel)
		}
		return in.sudoIfNeeded(ctx, "/bin/rm", rmArgs(p, dir)...)
	})
}

func rmArgs(path string, recursive bool) []string {
	args := []string{"-f", "--", path}
	if recursive {
		return append([]string{"-R"}, args...)
	}
	return args
}

func (in *Installer) gainPermissions(ctx context.Context, path string, flags []string, try func(path string) error) error {
	triedPermissions, triedOwnership := false, false
	for {
		err := try(path)
		if err == nil {
			return nil
		}
		target := filepath.Clean(path)
		if fi, e := os.Lstat(target); e == nil && fi.Mode()&fs.ModeSymlink != 0 {
			flags = []string{"-h"}
		}
		if !triedPermissions {
			_, _ = in.system(ctx, "/usr/bin/chflags", append(slices.Clone(flags), "--", "000", target)...)
			_, _ = in.system(ctx, "/bin/chmod", append(slices.Clone(flags), "--", "u+rwx", target)...)
			_, _ = in.system(ctx, "/bin/chmod", append(slices.Clone(flags), "-N", target)...)
			triedPermissions = true
			continue
		}
		if triedOwnership || !ownedByOther(target, slices.Contains(flags, "-R")) {
			return err
		}
		in.logf("==> Using sudo to gain ownership of path '%s'", path)
		chown := in.command(sudo.Sudo, "chown", append(slices.Clone(flags), "--", currentUser(), target)...)
		chown.MustSucceed = false
		if _, e := in.capture(ctx, chown); e != nil {
			return e
		}
		triedOwnership, triedPermissions = true, false
	}
}

func (in *Installer) gainPermissionsRemove(ctx context.Context, path string) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return nil
	}
	flags, dir := []string(nil), false
	if fi.Mode()&fs.ModeSymlink != 0 {
		flags = []string{"-h"}
	} else if fi.IsDir() {
		flags, dir = []string{"-R"}, true
	}
	return in.gainPermissions(ctx, path, flags, func(p string) error {
		if !writable(filepath.Dir(p)) {
			return in.sudoIfNeeded(ctx, "/bin/rm", rmArgs(p, dir)...)
		}
		if dir {
			return os.RemoveAll(p) //fsx:allow brew's FileUtils.rm_r on a path the cask named
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) { //fsx:allow brew's FileUtils.rm_f on a path the cask named
			return err
		}
		return nil
	})
}

func (in *Installer) gainPermissionsRmdir(ctx context.Context, path string) error {
	return in.gainPermissions(ctx, path, []string{"-h"}, func(p string) error {
		if writable(filepath.Dir(p)) {
			return syscall.Rmdir(p) //fsx:allow brew's FileUtils.rmdir on a path the cask named
		}
		return in.sudoIfNeeded(ctx, "rmdir", "--", p)
	})
}

func (in *Installer) mkdirPath(ctx context.Context, r *fsx.Root, rel string) error {
	abs, err := r.Abs(rel)
	if err != nil {
		return err
	}
	existing := abs
	for {
		if fi, err := os.Stat(existing); err == nil && fi.IsDir() {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	if existing == abs {
		return nil
	}
	if writable(existing) {
		return r.MkdirAll(rel, 0o755)
	}
	return in.sudoIfNeeded(ctx, "/bin/mkdir", "-p", "--", abs)
}

var ownedByOther = ownedByAnother

func ownedByAnother(path string, recursive bool) bool {
	if os.Geteuid() == 0 {
		return false
	}
	other := false
	check := func(p string) {
		if fi, err := os.Lstat(p); err == nil {
			if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Geteuid() {
				other = true
			}
		}
	}
	if !recursive {
		check(path)
		return other
	}
	_ = filepath.WalkDir(path, func(p string, _ fs.DirEntry, err error) error {
		if err == nil {
			check(p)
		}
		if other {
			return filepath.SkipAll
		}
		return nil
	})
	return other
}

func currentUser() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}
