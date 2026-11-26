package poststep

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// A step may name any path, as brew's do. The prefix goes through the journaled
// root and everything else straight to the filesystem.

func (r *runner) within(abs string) (string, bool) {
	rel, err := filepath.Rel(r.c.Prefix, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

func (r *runner) mkdirAll(abs string) error {
	if rel, ok := r.within(abs); ok {
		return r.c.Root.MkdirAll(rel, 0o755)
	}
	return os.MkdirAll(abs, 0o755) //fsx:allow brew's Pathname#mkpath on a path the step named
}

func (r *runner) mkdir(abs string) error {
	if rel, ok := r.within(abs); ok {
		return r.c.Root.Mkdir(rel, 0o755)
	}
	return os.Mkdir(abs, 0o755) //fsx:allow brew's Pathname#mkdir on a path the step named
}

func (r *runner) writeFile(abs string, data []byte, perm fs.FileMode) error {
	if rel, ok := r.within(abs); ok {
		return r.c.Root.WriteFile(rel, data, perm)
	}
	return os.WriteFile(abs, data, perm) //fsx:allow brew's File write on a path the step named
}

func (r *runner) writeFrom(abs string, src io.Reader, perm fs.FileMode) error {
	if rel, ok := r.within(abs); ok {
		return r.c.Root.WriteFrom(rel, src, perm, time.Time{})
	}
	f, err := os.OpenFile(abs, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //fsx:allow brew's FileUtils.cp on a path the step named
	if err != nil {
		return err
	}
	_, err = io.Copy(f, src)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(abs, perm) //fsx:allow brew's FileUtils.cp on a path the step named
	}
	return err
}

func (r *runner) symlinkAt(text, abs string) error {
	if rel, ok := r.within(abs); ok {
		return r.c.Root.SymlinkAny(text, rel)
	}
	return os.Symlink(text, abs) //fsx:allow brew's File.symlink on a path the step named
}

func (r *runner) touchAt(abs string) error {
	if err := r.mkdirAll(filepath.Dir(abs)); err != nil {
		return err
	}
	if _, err := os.Lstat(abs); err != nil {
		return r.writeFile(abs, nil, 0o644)
	}
	now := time.Now()
	if rel, ok := r.within(abs); ok {
		return r.c.Root.Chtimes(rel, now, now)
	}
	return os.Chtimes(abs, now, now) //fsx:allow brew's FileUtils.touch on a path the step named
}

func (r *runner) removeAny(abs string) error {
	fi, err := os.Lstat(abs)
	if err != nil || fi.IsDir() {
		return nil
	}
	if rel, ok := r.within(abs); ok {
		return r.c.Root.Remove(rel)
	}
	return os.Remove(abs) //fsx:allow brew's FileUtils.rm_f on a path the step named
}

func (r *runner) removeTree(abs string) error {
	if rel, ok := r.within(abs); ok {
		return r.c.Root.RemoveAll(rel)
	}
	return os.RemoveAll(abs) //fsx:allow brew's FileUtils.rm_rf on a path the step named
}

// rename is FileUtils.mv: across file systems, and into or out of the prefix, it copies and removes.
func (r *runner) rename(source, dest string) error {
	sRel, sIn := r.within(source)
	dRel, dIn := r.within(dest)
	switch {
	case sIn && dIn:
		return r.c.Root.Rename(sRel, dRel)
	case !sIn && !dIn:
		err := os.Rename(source, dest) //fsx:allow brew's FileUtils.mv on paths the step named
		if !errors.Is(err, syscall.EXDEV) {
			return err
		}
	}
	if err := r.copyTree(source, dest); err != nil {
		return err
	}
	return r.removeTree(source)
}

func (r *runner) copyFile(source, dest string) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return r.writeFrom(dest, f, fi.Mode().Perm())
}

func (r *runner) copyTree(source, dest string) error {
	return filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sub, _ := filepath.Rel(source, p)
		dst := filepath.Join(dest, sub)
		switch {
		case d.IsDir():
			return r.mkdirAll(dst)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return r.symlinkAt(target, dst)
		}
		return r.copyFile(p, dst)
	})
}

func (r *runner) readlink(s step, abs string) (string, error) {
	got, err := os.Readlink(abs)
	if !errors.Is(err, fs.ErrPermission) || s.Sudo == sudoNever {
		return got, err
	}
	res, err := r.sudo(readlinkPath, "-n", abs)
	return res.Stdout, err
}
