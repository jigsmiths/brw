package poststep

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/jigsmiths/brw/internal/sudo"
)

const (
	lnPath       = "/bin/ln"
	readlinkPath = "/usr/bin/readlink"
	accessWrite  = 0x2
)

// symlinkWithSudo may link outside the prefix, as brew's symlink step does.
func (r *runner) symlinkWithSudo(s step) error {
	if s.Target == nil {
		return errors.New("the step has no path")
	}
	target, err := r.resolve(*s.Target)
	if err != nil {
		return err
	}
	if s.Source == nil {
		return errors.New("the step has no source")
	}
	if s.SourceGlob {
		sources, err := r.glob(*s.Source)
		if err != nil {
			return err
		}
		sources = slices.DeleteFunc(sources, func(p string) bool { _, err := os.Lstat(p); return err != nil })
		if len(sources) == 0 {
			return nil
		}
		if fi, err := os.Stat(target); len(sources) > 1 || (err == nil && fi.IsDir()) {
			if err := r.mkdirAll(target); err != nil {
				return err
			}
			for _, src := range sources {
				if err := r.createLink(s, src, filepath.Join(target, filepath.Base(src))); err != nil {
					return err
				}
			}
			return nil
		}
		return r.createLink(s, sources[0], target)
	}
	text := r.expand(s.Source.Path)
	if s.Source.Base != "relative" {
		if text, err = r.resolve(*s.Source); err != nil {
			return err
		}
	}
	return r.createLink(s, text, target)
}

func (r *runner) needsSudo(s step, dir string) bool {
	return s.Sudo == sudoAlways || (s.Sudo == sudoIfNeeded && syscall.Access(dir, accessWrite) != nil)
}

func (r *runner) createLink(s step, source, target string) error {
	dir := filepath.Dir(target)
	if err := r.mkdirAll(dir); err != nil {
		return err
	}
	if r.needsSudo(s, dir) {
		args := []string{"-s"}
		if s.Force {
			args = append(args, "-f")
		}
		_, err := r.sudo(lnPath, append(args, source, target)...)
		return err
	}
	if s.Force {
		_ = r.removeAny(target)
	}
	if rel, err := r.inside(target); err == nil {
		return r.c.Root.Symlink(source, rel)
	}
	return os.Symlink(source, target) //fsx:allow brew's File.symlink on a path the step named
}

func (r *runner) sudo(exe string, args ...string) (sudo.Result, error) {
	run := r.c.Sudo
	if run == nil {
		run = sudo.Exec
	}
	return run(r.ctx, sudo.Command{
		Exe: exe, Args: args, Privilege: sudo.Sudo, MustSucceed: true, Secrets: sudo.SensitiveValues(),
	})
}

func (r *runner) mkdirAll(abs string) error {
	if rel, err := r.inside(abs); err == nil {
		return r.c.Root.MkdirAll(rel, 0o755)
	}
	return os.MkdirAll(abs, 0o755) //fsx:allow brew's Pathname#mkpath on a path the step named
}

// removeAny is brew's FileUtils.rm_f: a missing path and a directory stay as they are.
func (r *runner) removeAny(abs string) error {
	fi, err := os.Lstat(abs)
	if err != nil || fi.IsDir() {
		return nil
	}
	if rel, err := r.inside(abs); err == nil {
		return r.c.Root.Remove(rel)
	}
	return os.Remove(abs) //fsx:allow brew's FileUtils.rm_f on a path the step named
}

// readlink falls back to sudo when the link is unreadable, as brew's read_symlink does.
func (r *runner) readlink(abs string) (string, error) {
	got, err := os.Readlink(abs)
	if !errors.Is(err, fs.ErrPermission) {
		return got, err
	}
	res, err := r.sudo(readlinkPath, "-n", abs)
	return res.Stdout, err
}
