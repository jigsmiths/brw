package install

import (
	"bytes"
	"errors"
	"io/fs"
	"path"
)

// restoreEtcVar copies the keg's .bottle/etc and .bottle/var into the prefix, as
// brew's Formula#install_etc_var does. A file that differs from one already in
// the prefix is kept, and the bottled one lands beside it as <name>.default.
func (in *Installer) restoreEtcVar(keg string) error {
	for _, top := range []string{"etc", "var"} {
		if err := in.restoreTree(path.Join(keg, ".bottle", top), top); err != nil {
			return err
		}
	}
	return nil
}

func (in *Installer) restoreTree(src, dst string) error {
	entries, err := in.Root.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := in.Root.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		s, d := path.Join(src, e.Name()), path.Join(dst, e.Name())
		switch {
		case e.IsDir():
			err = in.restoreTree(s, d)
		case e.Type()&fs.ModeSymlink != 0:
			err = in.restoreLink(s, d)
		default:
			err = in.restoreFile(s, d, e)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (in *Installer) restoreLink(src, dst string) error {
	if _, err := in.Root.Lstat(dst); err == nil {
		return nil
	}
	target, err := in.Root.Readlink(src)
	if err != nil {
		return err
	}
	return in.Root.Symlink(target, dst)
}

func (in *Installer) restoreFile(src, dst string, e fs.DirEntry) error {
	data, err := in.Root.ReadFile(src)
	if err != nil {
		return err
	}
	info, err := e.Info()
	if err != nil {
		return err
	}
	perm := info.Mode().Perm()
	if old, err := in.Root.ReadFile(dst); err == nil {
		if bytes.Equal(old, data) {
			return nil
		}
		dst += ".default"
		if _, err := in.Root.Lstat(dst); err == nil {
			return nil
		}
	}
	return in.Root.WriteFile(dst, data, perm)
}
