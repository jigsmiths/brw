package link

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/fsx"
)

// newLinker returns a Linker over a fresh prefix with the directories brew
// always has.
func newLinker(t *testing.T) (*Linker, string) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"Cellar", "opt", "bin", "etc", "include", "lib", "sbin", "share", "var/homebrew/linked"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return &Linker{Root: root, Prefix: dir}, dir
}

// file is a file or, when it ends in "->target", a symlink; a name that ends in
// a slash is an empty directory.
type files []string

// install creates a keg Cellar/name/version holding the given entries.
func install(t *testing.T, prefix, name, version string, entries files) Keg {
	t.Helper()
	keg := filepath.Join(prefix, "Cellar", name, version)
	for _, e := range entries {
		path, target, isLink := strings.Cut(e, "->")
		path = strings.TrimSpace(path)
		full := filepath.Join(keg, path)
		switch {
		case isLink:
			mustMkdir(t, filepath.Dir(full))
			if err := os.Symlink(strings.TrimSpace(target), full); err != nil {
				t.Fatal(err)
			}
		case strings.HasSuffix(path, "/"):
			mustMkdir(t, full)
		default:
			mustMkdir(t, filepath.Dir(full))
			if err := os.WriteFile(full, []byte(name+" "+path), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	mustMkdir(t, keg)
	return Keg{Name: name, Version: version}
}

func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

// tree lists everything in the prefix outside the Cellar as "path" for a
// directory (with a trailing slash), "path -> target" for a symlink, and
// "path" for a file.
func tree(t *testing.T, prefix string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(prefix, p)
		if rel == "." || rel == "Cellar" || strings.HasPrefix(rel, "Cellar"+string(filepath.Separator)) {
			return nil
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			out = append(out, rel+" -> "+target)
		case d.IsDir():
			out = append(out, rel+"/")
		default:
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// links is tree without the empty directories every prefix starts with.
func links(t *testing.T, prefix string) []string {
	t.Helper()
	base := map[string]bool{"opt/": true, "bin/": true, "etc/": true, "include/": true, "lib/": true, "sbin/": true, "share/": true, "var/": true, "var/homebrew/": true, "var/homebrew/linked/": true}
	var out []string
	for _, e := range tree(t, prefix) {
		if !base[e] {
			out = append(out, e)
		}
	}
	return out
}
