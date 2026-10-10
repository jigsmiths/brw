// Package fsx is the only way brw changes files in a prefix. Changes stay
// inside the root, never overwrite, and are journaled.
package fsx

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var (
	ErrExists = errors.New("path already exists")
	ErrRoot   = errors.New("refusing to remove the root")
)

type Root struct {
	dir     string
	root    *os.Root
	journal io.Writer
}

// Open confines operations to dir. journal may be nil.
func Open(dir string, journal io.Writer) (*Root, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(abs)
	if err != nil {
		return nil, err
	}
	return &Root{dir: abs, root: root, journal: journal}, nil
}

func (r *Root) Close() error { return r.root.Close() }

// WriteFile atomically creates name; ErrExists if it exists.
func (r *Root) WriteFile(name string, data []byte, perm fs.FileMode) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if err := r.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	tmp, err := r.writeTemp(filepath.Dir(rel), data, perm)
	if err != nil {
		return err
	}
	defer func() { _ = r.root.Remove(tmp) }()
	// Link fails if rel exists, unlike Rename, which would replace it.
	if err := r.root.Link(tmp, rel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", rel, ErrExists)
		}
		return err
	}
	return r.log("write %s", rel)
}

func (r *Root) writeTemp(dir string, data []byte, perm fs.FileMode) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".brw-tmp-"+hex.EncodeToString(b[:]))
	f, err := r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = r.root.Chmod(tmp, perm)
	}
	if err != nil {
		_ = r.root.Remove(tmp)
		return "", err
	}
	return tmp, nil
}

// OpenLock opens name for locking, creating it if needed. Not journaled.
func (r *Root) OpenLock(name string) (*os.File, error) {
	rel, err := r.rel(name)
	if err != nil {
		return nil, err
	}
	if err := r.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return nil, err
	}
	// os.Root follows symlinks inside the root even with O_NOFOLLOW.
	if fi, err := r.root.Lstat(rel); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s: is a symlink", rel)
	}
	return r.root.OpenFile(rel, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o644)
}

func (r *Root) Lstat(name string) (fs.FileInfo, error) {
	rel, err := r.rel(name)
	if err != nil {
		return nil, err
	}
	return r.root.Lstat(rel)
}

// Symlink creates name -> target. The target must stay inside the root;
// ErrExists if name exists.
func (r *Root) Symlink(target, name string) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if err := r.checkTarget(target, rel); err != nil {
		return err
	}
	if err := r.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := r.root.Symlink(target, rel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", rel, ErrExists)
		}
		return err
	}
	return r.log("symlink %s -> %s", rel, target)
}

func (r *Root) Remove(name string) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if rel == "." {
		return ErrRoot
	}
	if err := r.root.Remove(rel); err != nil {
		return err
	}
	return r.log("remove %s", rel)
}

// RemoveAll does not follow symlinks.
func (r *Root) RemoveAll(name string) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if rel == "." {
		return ErrRoot
	}
	if err := r.root.RemoveAll(rel); err != nil {
		return err
	}
	return r.log("removeall %s", rel)
}

func (r *Root) rel(name string) (string, error) {
	if filepath.IsAbs(name) {
		rel, err := filepath.Rel(r.dir, name)
		if err != nil {
			return "", err
		}
		name = rel
	}
	name = filepath.Clean(name)
	if escapes(name) {
		return "", fmt.Errorf("%s: outside the root", name)
	}
	return name, nil
}

func (r *Root) checkTarget(target, rel string) error {
	if filepath.IsAbs(target) {
		if _, err := r.rel(target); err != nil {
			return fmt.Errorf("symlink target %s: outside the root", target)
		}
		return nil
	}
	if escapes(filepath.Join(filepath.Dir(rel), target)) {
		return fmt.Errorf("symlink target %s: outside the root", target)
	}
	return nil
}

func escapes(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (r *Root) log(format string, args ...any) error {
	if r.journal == nil {
		return nil
	}
	_, err := fmt.Fprintf(r.journal, format+"\n", args...)
	return err
}
