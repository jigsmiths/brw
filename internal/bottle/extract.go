package bottle

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
)

// Extract unpacks a bottle into dest (relative to the prefix, must not exist)
// and leaves the keg at dest/name/version. dest is removed on any failure.
//
// The archive is untrusted and must hold exactly that keg. Extract refuses
// paths that climb out, devices, setuid bits, symlinks that would leave the
// prefix once the keg is in the Cellar, hard links to anything but an earlier
// file of the keg, and entries whose parent it did not create itself, so
// nothing is written through a symlink.
func Extract(root *fsx.Root, tarball, dest, name, version string) (err error) {
	if err := root.MkdirAll(path.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := root.Mkdir(dest, 0o755); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = root.RemoveAll(dest)
		}
	}()

	f, err := os.Open(tarball)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	x := &extractor{root: root, dest: dest, keg: name + "/" + version, dirs: map[string]bool{}, files: map[string]bool{}}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if err := x.entry(h, tr); err != nil {
			return fmt.Errorf("%s: %w", h.Name, err)
		}
	}
	// Reading to the end checks the gzip checksum.
	if _, err := io.Copy(io.Discard, gz); err != nil {
		return err
	}
	if !x.dirs[x.keg] {
		return fmt.Errorf("the archive has no %s", x.keg)
	}
	return x.finish()
}

type dirMode struct {
	rel   string
	mode  os.FileMode
	mtime tar.Header
}

type extractor struct {
	root  *fsx.Root
	dest  string
	keg   string          // name/version
	dirs  map[string]bool // directories this extraction created
	files map[string]bool // regular files it created, valid hard link targets
	modes []dirMode       // applied last, deepest first
}

func (x *extractor) abs(rel string) string { return path.Join(x.dest, rel) }

func (x *extractor) entry(h *tar.Header, r io.Reader) error {
	rel, err := cleanName(h.Name)
	if err != nil {
		return err
	}
	if rel != x.keg && !strings.HasPrefix(rel, x.keg+"/") {
		return fmt.Errorf("outside %s", x.keg)
	}
	if h.Mode&0o7000 != 0 {
		return fmt.Errorf("mode %o has setuid, setgid or sticky bits", h.Mode)
	}
	perm := os.FileMode(h.Mode&0o777) &^ umask()
	switch h.Typeflag {
	case tar.TypeDir:
		return x.mkdir(rel, perm, h)
	case tar.TypeReg:
		return x.file(rel, perm, h, r)
	case tar.TypeSymlink:
		return x.symlink(rel, h.Linkname)
	case tar.TypeLink:
		return x.hardlink(rel, h.Linkname)
	}
	return fmt.Errorf("unsupported entry type %q", h.Typeflag)
}

// cleanName accepts only a plain relative path: no dot or empty components,
// nothing absolute, no null bytes. A trailing slash on a directory is allowed.
func cleanName(name string) (string, error) {
	trimmed := strings.TrimSuffix(name, "/")
	if trimmed == "" || strings.ContainsRune(trimmed, 0) || path.IsAbs(trimmed) || path.Clean(trimmed) != trimmed ||
		trimmed == "." || trimmed == ".." || strings.HasPrefix(trimmed, "../") {
		return "", errors.New("unsafe path")
	}
	return trimmed, nil
}

// ensureParents creates the directories above rel that the archive did not list.
func (x *extractor) ensureParents(rel string) error {
	var missing []string
	for dir := path.Dir(rel); dir != "."; dir = path.Dir(dir) {
		if x.dirs[dir] {
			break
		}
		missing = append(missing, dir)
	}
	for _, dir := range slices.Backward(missing) {
		if err := x.root.Mkdir(x.abs(dir), 0o755); err != nil {
			return err
		}
		x.dirs[dir] = true
	}
	return nil
}

func (x *extractor) mkdir(rel string, perm os.FileMode, h *tar.Header) error {
	if err := x.ensureParents(rel); err != nil {
		return err
	}
	if !x.dirs[rel] {
		if err := x.root.Mkdir(x.abs(rel), 0o755); err != nil {
			return err
		}
		x.dirs[rel] = true
	}
	x.modes = append(x.modes, dirMode{rel, perm, *h})
	return nil
}

func (x *extractor) file(rel string, perm os.FileMode, h *tar.Header, r io.Reader) error {
	if err := x.ensureParents(rel); err != nil {
		return err
	}
	if err := x.root.WriteFrom(x.abs(rel), r, perm, h.ModTime); err != nil {
		return err
	}
	x.files[rel] = true
	return nil
}

// symlink allows relative targets that stay inside the prefix when resolved
// from where the keg will live, which is under Cellar.
func (x *extractor) symlink(rel, target string) error {
	if target == "" || strings.ContainsRune(target, 0) || path.IsAbs(target) {
		return fmt.Errorf("symlink target %q is not a relative path", target)
	}
	final := path.Join("Cellar", path.Dir(rel), target)
	if final == ".." || strings.HasPrefix(final, "../") {
		return fmt.Errorf("symlink target %q leaves the prefix", target)
	}
	if err := x.ensureParents(rel); err != nil {
		return err
	}
	return x.root.Symlink(target, x.abs(rel))
}

func (x *extractor) hardlink(rel, target string) error {
	target, err := cleanName(target)
	if err != nil {
		return fmt.Errorf("hard link target: %w", err)
	}
	if !x.files[target] {
		return fmt.Errorf("hard link to %s, which is not an earlier file of the keg", target)
	}
	if err := x.ensureParents(rel); err != nil {
		return err
	}
	if err := x.root.Link(x.abs(target), x.abs(rel)); err != nil {
		return err
	}
	x.files[rel] = true
	return nil
}

// finish applies directory modes and times once their contents are in place.
func (x *extractor) finish() error {
	for _, d := range slices.Backward(x.modes) {
		if err := x.root.Chmod(x.abs(d.rel), d.mode); err != nil {
			return err
		}
		if err := x.root.Chtimes(x.abs(d.rel), d.mtime.ModTime, d.mtime.ModTime); err != nil {
			return err
		}
	}
	return nil
}

var (
	umaskOnce sync.Once
	umaskBits os.FileMode
)

// umask is what tar run by brew applies to modes: brew sets `umask go-w` on
// top of the user's own.
func umask() os.FileMode {
	umaskOnce.Do(func() {
		old := syscall.Umask(0)
		syscall.Umask(old)
		umaskBits = os.FileMode(old) | 0o022
	})
	return umaskBits
}
