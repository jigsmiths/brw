// Package link puts the files of a keg into the prefix as symlinks and takes
// them out again, following brew's rules for what is linked and how.
package link

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/fsx"
)

type Keg struct {
	Name, Version string
	Aliases       []string // from the receipt
}

func (k Keg) path() string   { return "Cellar/" + k.Name + "/" + k.Version }
func (k Keg) rack() string   { return "Cellar/" + k.Name }
func (k Keg) opt() string    { return "opt/" + k.Name }
func (k Keg) record() string { return "var/homebrew/linked/" + k.Name }

type Linker struct {
	Root   *fsx.Root
	Prefix string // absolute path of Root, to read absolute symlinks into it
}

// The directories of the prefix that stay when the last link in them goes.
var mustExist = []string{"bin", "etc", "include", "lib", "sbin", "share", "opt", "var/homebrew/linked"}

// unlinkDirs are the directories of a keg that Unlink looks through.
var unlinkDirs = []string{"bin", "etc", "include", "lib", "sbin", "share", "var"}

// relPath is the path of target as seen from dir, both relative to the prefix.
func relPath(dir, target string) string {
	if dir == "." || dir == "" {
		return target
	}
	up := strings.Count(path.Clean(dir), "/") + 1
	return strings.Repeat("../", up) + target
}

// resolved is where the symlink p points, cleaned lexically, or p itself if it
// is not a symlink.
func (l *Linker) resolved(p string) string {
	target, err := l.Root.Readlink(p)
	if err != nil {
		return p
	}
	if path.IsAbs(target) {
		if rel, ok := strings.CutPrefix(path.Clean(target), l.Prefix+"/"); ok {
			return rel
		}
		return path.Clean(target)
	}
	return path.Clean(path.Join(path.Dir(p), target))
}

func (l *Linker) isSymlink(p string) bool {
	fi, err := l.Root.Lstat(p)
	return err == nil && fi.Mode()&fs.ModeSymlink != 0
}

// isDir is true for a real directory, not a symlink to one.
func (l *Linker) isDir(p string) bool {
	fi, err := l.Root.Lstat(p)
	return err == nil && fi.IsDir()
}

// stat follows symlinks inside the prefix, up to a depth that rules out loops.
func (l *Linker) stat(p string) (fs.FileInfo, error) {
	for range 40 {
		fi, err := l.Root.Lstat(p)
		if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
			return fi, err
		}
		p = l.resolved(p)
		if path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return nil, fs.ErrNotExist
		}
	}
	return nil, errors.New("too many levels of symbolic links")
}

func (l *Linker) exists(p string) bool {
	_, err := l.stat(p)
	return err == nil
}

// realpath resolves every symlink in p, which has to stay inside the prefix.
func (l *Linker) realpath(p string) (string, bool) {
	for range 40 {
		fi, err := l.Root.Lstat(p)
		if err != nil {
			return "", false
		}
		if fi.Mode()&fs.ModeSymlink == 0 {
			return p, true
		}
		p = l.resolved(p)
		if path.IsAbs(p) || p == ".." || strings.HasPrefix(p, "../") {
			return "", false
		}
	}
	return "", false
}

// rmdirIfPossible removes an empty directory, or one that holds only a
// .DS_Store, and does nothing otherwise.
func (l *Linker) rmdirIfPossible(dir string) {
	if !l.isDir(dir) {
		return
	}
	if l.Root.Remove(dir) == nil {
		return
	}
	entries, err := l.Root.ReadDir(dir)
	if err == nil && len(entries) == 1 && entries[0].Name() == ".DS_Store" {
		if l.Root.Remove(path.Join(dir, ".DS_Store")) == nil {
			_ = l.Root.Remove(dir)
		}
	}
}

// Linked reports whether the keg is the one the linked record names.
func (l *Linker) Linked(k Keg) bool {
	if !l.isSymlink(k.record()) {
		return false
	}
	fi, err := l.stat(k.record())
	return err == nil && fi.IsDir() && l.resolved(k.record()) == k.path()
}

// makeRelativeSymlink links dst to src. A link that already points there is
// left alone and a dangling one in the way is replaced.
func (l *Linker) makeRelativeSymlink(dst, src string) error {
	if l.isSymlink(dst) && l.resolved(dst) == src {
		return nil
	}
	for range 2 {
		if err := l.Root.MkdirAll(path.Dir(dst), 0o755); err != nil {
			return &LinkError{Dst: dst, Src: src, Err: err}
		}
		err := l.Root.Symlink(relPath(path.Dir(dst), src), dst)
		switch {
		case err == nil:
			return nil
		case !errors.Is(err, fsx.ErrExists):
			return &LinkError{Dst: dst, Src: src, Err: err}
		case l.exists(dst):
			return &ConflictError{Dst: dst, Src: src, Owner: l.owner(dst)}
		case l.isSymlink(dst): // dangling
			if err := l.Root.Remove(dst); err != nil {
				return &LinkError{Dst: dst, Src: src, Err: err}
			}
		default:
			return &LinkError{Dst: dst, Src: src, Err: err}
		}
	}
	return &LinkError{Dst: dst, Src: src, Err: errors.New("could not replace a dangling symlink")}
}

// owner names the keg that a symlink at p belongs to, or "".
func (l *Linker) owner(p string) string {
	if !l.isSymlink(p) {
		return ""
	}
	if name, _, ok := kegOf(l.resolved(p)); ok {
		return name
	}
	return ""
}

// kegOf splits a path inside the Cellar into the keg's name, version and the
// rest, which is "" for the keg itself.
func kegOf(p string) (name, version string, ok bool) {
	rest, ok := strings.CutPrefix(p, "Cellar/")
	if !ok {
		return "", "", false
	}
	parts := strings.SplitN(rest, "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// Optlink points opt/NAME, and opt/ALIAS for each alias, at the keg.
func (l *Linker) Optlink(k Keg) error {
	if err := l.removeOldAliases(k); err != nil {
		return err
	}
	if err := l.replaceRecord(k.opt(), k.path()); err != nil {
		return err
	}
	for _, a := range k.Aliases {
		if err := l.replaceRecord("opt/"+a, k.path()); err != nil {
			return err
		}
	}
	// Other records in opt/ that lead into this formula's rack, such as the
	// ones of an older alias, follow the keg.
	entries, err := l.Root.ReadDir("opt")
	if err != nil {
		return err
	}
	for _, e := range entries {
		rec := "opt/" + e.Name()
		if rec == k.opt() || !l.isSymlink(rec) {
			continue
		}
		if fi, err := l.stat(rec); err != nil || !fi.IsDir() {
			continue
		}
		if path.Dir(l.resolved(rec)) != k.rack() {
			continue
		}
		if err := l.replaceRecord(rec, k.path()); err != nil {
			return err
		}
	}
	return nil
}

func (l *Linker) replaceRecord(record, kegPath string) error {
	if l.isSymlink(record) || l.exists(record) {
		if err := l.Root.Remove(record); err != nil {
			return &LinkError{Dst: record, Src: kegPath, Err: err}
		}
	}
	return l.makeRelativeSymlink(record, kegPath)
}

var versionedAlias = regexp.MustCompile(`.+@.`)

// removeOldAliases deletes alias records that point at this formula and
// versioned records left behind by earlier releases.
func (l *Linker) removeOldAliases(k Keg) error {
	for _, a := range k.Aliases {
		if versionedAlias.MatchString(a) {
			continue
		}
		if err := l.removeAliasSymlink("opt/"+a, k.opt(), false); err != nil {
			return err
		}
		if err := l.removeAliasSymlink("var/homebrew/linked/"+a, k.record(), false); err != nil {
			return err
		}
	}
	entries, err := l.Root.ReadDir("opt")
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, k.Name+"@") {
			continue
		}
		if slices.Contains(k.Aliases, name) && l.exists("opt/"+name) {
			continue
		}
		if err := l.removeAliasSymlink("opt/"+name, k.rack(), true); err != nil {
			return err
		}
		if err := l.removeAliasSymlink("var/homebrew/linked/"+name, k.rack(), true); err != nil {
			return err
		}
	}
	return nil
}

func (l *Linker) removeAliasSymlink(alias, match string, matchParent bool) error {
	switch {
	case l.isSymlink(alias) && l.exists(alias):
		real, ok := l.realpath(alias)
		if !ok {
			return nil
		}
		if matchParent {
			real = path.Dir(real)
		}
		if matchReal, ok := l.realpath(match); ok && real == matchReal {
			return l.Root.Remove(alias)
		}
	case l.isSymlink(alias) || l.exists(alias):
		return l.Root.Remove(alias)
	}
	return nil
}

// Unlink removes the symlinks that lead into the keg, then emptied directories
// (except the standard ones) and the linked record. The opt record stays.
func (l *Linker) Unlink(k Keg) error {
	var dirs []string
	for _, d := range unlinkDirs {
		root := k.path() + "/" + d
		if !l.exists(root) {
			continue
		}
		var walkErr error
		l.walk(root, true, func(src string, isDir bool) bool {
			dst := strings.TrimPrefix(src, k.path()+"/")
			if l.isDir(dst) {
				dirs = append(dirs, dst)
			}
			if !l.isSymlink(dst) || l.resolved(dst) != src {
				return true
			}
			if err := l.Root.Remove(dst); err != nil {
				walkErr = fmt.Errorf("unlinking %s: %w", dst, err)
				return false
			}
			return !isDir
		})
		if walkErr != nil {
			return walkErr
		}
	}
	if err := l.removeOldAliases(k); err != nil {
		return err
	}
	if l.Linked(k) {
		if err := l.Root.Remove(k.record()); err != nil {
			return err
		}
		l.rmdirIfPossible(path.Dir(k.record()))
	}
	for _, d := range slices.Backward(dirs) {
		if !slices.Contains(mustExist, d) {
			l.rmdirIfPossible(d)
		}
	}
	return nil
}

// walk visits the entries below root (and root first if withRoot) in sorted
// order, parents first. visit says whether to go into a directory. Symlinks are
// never followed.
func (l *Linker) walk(root string, withRoot bool, visit func(p string, isDir bool) (descend bool)) {
	if withRoot {
		fi, err := l.Root.Lstat(root)
		if err != nil {
			return
		}
		if !visit(root, fi.IsDir()) || !fi.IsDir() {
			return
		}
	}
	entries, err := l.Root.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := root + "/" + e.Name()
		if e.IsDir() {
			if visit(p, true) {
				l.walk(p, false, visit)
			}
		} else {
			visit(p, false)
		}
	}
}

// ConflictError means a file or a link of another keg is in the way.
type ConflictError struct {
	Dst, Src string
	Owner    string // the formula whose link is in the way, if any
}

func (e *ConflictError) Error() string {
	if e.Owner != "" {
		return fmt.Sprintf("could not link %s: %s is a symlink belonging to %s", e.Src, e.Dst, e.Owner)
	}
	return fmt.Sprintf("could not link %s: %s already exists", e.Src, e.Dst)
}

// LinkError is any other failure to make a link.
type LinkError struct {
	Dst, Src string
	Err      error
}

func (e *LinkError) Error() string {
	return fmt.Sprintf("could not link %s to %s: %v", e.Dst, e.Src, e.Err)
}
func (e *LinkError) Unwrap() error { return e.Err }
