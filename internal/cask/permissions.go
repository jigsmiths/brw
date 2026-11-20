package cask

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
)

// sudoIfNeeded is brew's sudo: nil: the command as the user, then, if that
// fails, with sudo.
func (in *Installer) sudoIfNeeded(ctx context.Context, name string, args ...string) error {
	if _, err := in.exec(ctx, name, args...); err == nil {
		return nil
	}
	_, err := in.privileged(ctx, true, name, args...)
	return err
}

// removePath is brew's gain_permissions_remove for a path under r: removed
// through r when its folder is writable, else with rm and sudo if needed. A
// failure clears flags and ACLs and makes it writable, then, if someone else
// owns it, takes ownership with sudo, retrying after each.
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
	remove := func() error {
		if writable(filepath.Dir(abs)) {
			return r.RemoveAll(rel)
		}
		args := []string{"-f", "--", abs}
		if dir {
			args = append([]string{"-R"}, args...)
		}
		return in.sudoIfNeeded(ctx, "/bin/rm", args...)
	}
	fix := func() {
		_, _ = in.exec(ctx, "/usr/bin/chflags", append(flags, "--", "000", abs)...)
		_, _ = in.exec(ctx, "/bin/chmod", append(flags, "--", "u+rwx", abs)...)
		_, _ = in.exec(ctx, "/bin/chmod", append(flags, "-N", abs)...)
	}
	if err = remove(); err == nil {
		return nil
	}
	fix()
	if err = remove(); err == nil {
		return nil
	}
	if ownedByOther(abs, dir) {
		in.logf("==> Using sudo to gain ownership of path '%s'", abs)
		_, _ = in.privileged(ctx, true, "chown", append(flags, "--", currentUser(), abs)...)
		fix()
		err = remove()
	}
	return err
}

// mkdirPath is brew's gain_permissions_mkpath: through r when the nearest
// existing folder is writable, else mkdir -p with sudo if needed.
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

// ownedByOther says whether sudo chown could help: whether the path, or with
// recursive anything under it, belongs to someone else. Tests replace it.
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
