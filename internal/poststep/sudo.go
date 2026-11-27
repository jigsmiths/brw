package poststep

import (
	"errors"
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

func (r *runner) symlink(s step) error {
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
	return r.symlinkAt(source, target)
}

func (r *runner) sudo(exe string, args ...string) (sudo.Result, error) {
	return r.sudoCommand(sudo.Command{Exe: exe, Args: args, Privilege: sudo.Sudo, MustSucceed: true})
}

func (r *runner) sudoCommand(c sudo.Command) (sudo.Result, error) {
	run := r.c.Sudo
	if run == nil {
		run = sudo.Exec
	}
	c.Secrets = sudo.SensitiveValues()
	return run(r.ctx, c)
}
