// Package fsx is the only way brw changes files in a prefix. Changes stay
// inside the root, never overwrite, and are journaled.
package fsx

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"
)

// Staging is where a keg is unpacked and relocated before it is renamed into
// place. brew ignores directories that start with a dot in the Cellar.
const Staging = "Cellar/.brw-staging"

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

// WriteFile atomically creates name and syncs it to disk; ErrExists if it exists.
func (r *Root) WriteFile(name string, data []byte, perm fs.FileMode) error {
	return r.write(name, bytes.NewReader(data), perm)
}

// WriteFrom creates name from src without replacing anything; ErrExists if it
// exists. The parent must exist. It is for unpacking into the staging area, so
// unlike WriteFile it creates the file in place, without syncing: fsync costs
// about 8ms a file on APFS, and brew does not do it either. If reading or
// writing fails the file is removed. A zero mtime leaves the current time.
func (r *Root) WriteFrom(name string, src io.Reader, perm fs.FileMode, mtime time.Time) (err error) {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", rel, ErrExists)
		}
		return err
	}
	defer func() {
		if err != nil {
			_ = r.root.Remove(rel)
		}
	}()
	_, err = io.Copy(f, src)
	if err == nil {
		err = f.Chmod(perm)
	}
	if err == nil && !mtime.IsZero() {
		err = syscall.Futimes(int(f.Fd()), []syscall.Timeval{syscall.NsecToTimeval(mtime.UnixNano()), syscall.NsecToTimeval(mtime.UnixNano())})
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return r.log("write %s", rel)
}

func (r *Root) write(name string, src io.Reader, perm fs.FileMode) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if err := r.root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	tmp, err := r.writeTemp(filepath.Dir(rel), src, perm)
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

func (r *Root) writeTemp(dir string, src io.Reader, perm fs.FileMode) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tmp := filepath.Join(dir, ".brw-tmp-"+hex.EncodeToString(b[:]))
	f, err := r.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	_, err = io.Copy(f, src)
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

// Abs returns the absolute path of name, for handing to another program. It
// only checks that name is inside the root.
func (r *Root) Abs(name string) (string, error) {
	rel, err := r.rel(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(r.dir, rel), nil
}

// Readlink returns the target of a symlink in the root, as written.
func (r *Root) Readlink(name string) (string, error) {
	rel, err := r.rel(name)
	if err != nil {
		return "", err
	}
	return r.root.Readlink(rel)
}

// ReadDir lists a directory in the root, sorted by name. It does not follow
// symlinks among the entries.
func (r *Root) ReadDir(name string) ([]fs.DirEntry, error) {
	rel, err := r.rel(name)
	if err != nil {
		return nil, err
	}
	d, err := r.root.Open(rel)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	slices.SortFunc(entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, err
}

// ReadFile reads a file inside the root. It changes nothing and is not journaled.
func (r *Root) ReadFile(name string) ([]byte, error) {
	rel, err := r.rel(name)
	if err != nil {
		return nil, err
	}
	return r.root.ReadFile(rel)
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

// Mkdir creates one directory, whose parent must exist; ErrExists if name
// exists. perm is subject to the umask.
func (r *Root) Mkdir(name string, perm fs.FileMode) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if rel == "." {
		return fmt.Errorf("%s: %w", rel, ErrExists)
	}
	if err := r.root.Mkdir(rel, perm); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", rel, ErrExists)
		}
		return err
	}
	return r.log("mkdir %s", rel)
}

// MkdirAll creates name and any missing parents, and journals each directory it
// creates. Existing directories are left alone.
func (r *Root) MkdirAll(name string, perm fs.FileMode) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	cur := ""
	for part := range strings.SplitSeq(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		fi, err := r.root.Stat(cur)
		if err == nil {
			if !fi.IsDir() {
				return fmt.Errorf("%s: not a directory", cur)
			}
			continue
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := r.root.Mkdir(cur, perm); err != nil {
			return err
		}
		if err := r.log("mkdir %s", cur); err != nil {
			return err
		}
	}
	return nil
}

func (r *Root) Chtimes(name string, atime, mtime time.Time) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if err := r.root.Chtimes(rel, atime, mtime); err != nil {
		return err
	}
	return r.log("chtimes %s", rel)
}

func (r *Root) Chmod(name string, perm fs.FileMode) error {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if err := r.root.Chmod(rel, perm); err != nil {
		return err
	}
	return r.log("chmod %s %o", rel, perm.Perm())
}

// Link makes newname another name for the file oldname; ErrExists if newname exists.
func (r *Root) Link(oldname, newname string) error {
	oldRel, err := r.rel(oldname)
	if err != nil {
		return err
	}
	newRel, err := r.rel(newname)
	if err != nil {
		return err
	}
	if err := r.root.Link(oldRel, newRel); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("%s: %w", newRel, ErrExists)
		}
		return err
	}
	return r.log("link %s %s", oldRel, newRel)
}

// Rename moves oldname to newname without replacing anything; ErrExists if
// newname exists, even as an empty directory.
func (r *Root) Rename(oldname, newname string) error {
	oldRel, err := r.rel(oldname)
	if err != nil {
		return err
	}
	newRel, err := r.rel(newname)
	if err != nil {
		return err
	}
	if _, err := r.root.Lstat(newRel); err == nil {
		return fmt.Errorf("%s: %w", newRel, ErrExists)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := r.root.Rename(oldRel, newRel); err != nil {
		return err
	}
	return r.log("rename %s %s", oldRel, newRel)
}

// Replace rewrites the contents of an existing regular file in the staging
// area, in place so that hard links see the change. It is the only call that
// overwrites, and it refuses any path outside Staging. The file is not changed
// atomically: a keg that fails halfway is discarded, never installed.
func (r *Root) Replace(name string, data []byte) (err error) {
	rel, err := r.rel(name)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(rel, Staging+string(filepath.Separator)) {
		return fmt.Errorf("%s: not in the staging area", rel)
	}
	fi, err := r.root.Lstat(rel)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s: not a regular file", rel)
	}
	if perm := fi.Mode().Perm(); perm&0o200 == 0 {
		if err := r.root.Chmod(rel, perm|0o200); err != nil {
			return err
		}
		defer func() { err = errors.Join(err, r.root.Chmod(rel, perm)) }()
	}
	f, err := r.root.OpenFile(rel, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return r.log("replace %s", rel)
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
