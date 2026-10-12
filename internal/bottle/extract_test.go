package bottle

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/fsx"
)

type entry struct {
	name, link, body string
	typ              byte
	mode             int64
	mtime            time.Time
}

var epoch = time.Date(2026, 6, 20, 15, 36, 55, 0, time.UTC)

func dir(name string, mode int64) entry { return entry{name: name, typ: tar.TypeDir, mode: mode} }
func file(name, body string, mode int64) entry {
	return entry{name: name, typ: tar.TypeReg, body: body, mode: mode}
}
func symlink(name, target string) entry {
	return entry{name: name, typ: tar.TypeSymlink, link: target, mode: 0o777}
}
func hardlink(name, target string) entry {
	return entry{name: name, typ: tar.TypeLink, link: target, mode: 0o644}
}

// tarball writes entries to a gzipped tar and returns its path.
func tarball(t *testing.T, entries ...entry) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, ModTime: epoch, Size: int64(len(e.body))}
		if !e.mtime.IsZero() {
			h.ModTime = e.mtime
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "bottle.tar.gz")
	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func prefix(t *testing.T) (*fsx.Root, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "Cellar"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r, dir
}

const stage = fsx.Staging + "/id"

func extract(t *testing.T, entries ...entry) (string, error) {
	t.Helper()
	r, dir := prefix(t)
	err := Extract(r, tarball(t, entries...), stage, "jq", "1.8.2")
	return dir, err
}

func TestExtract(t *testing.T) {
	root, err := extract(t,
		dir("jq/1.8.2", 0o755),
		dir("jq/1.8.2/bin", 0o755),
		file("jq/1.8.2/bin/jq", "binary", 0o555),
		file("jq/1.8.2/COPYING", "license", 0o444),
		dir("jq/1.8.2/lib", 0o755),
		file("jq/1.8.2/lib/libjq.1.dylib", "lib", 0o755),
		symlink("jq/1.8.2/lib/libjq.dylib", "libjq.1.dylib"),
		hardlink("jq/1.8.2/lib/libjq.copy", "jq/1.8.2/lib/libjq.1.dylib"),
		symlink("jq/1.8.2/etc-cert", "../../../etc/ca-certificates/cert.pem"), // leaves the keg, stays in the prefix
	)
	if err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(root, stage, "jq/1.8.2")
	check := func(rel, body string, mode fs.FileMode) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(keg, rel))
		fi, _ := os.Stat(filepath.Join(keg, rel))
		if err != nil || string(got) != body || fi.Mode().Perm() != mode {
			t.Errorf("%s = %q mode %v, %v; want %q mode %v", rel, got, fi.Mode().Perm(), err, body, mode)
		}
		if !fi.ModTime().Equal(epoch) {
			t.Errorf("%s mtime = %v, want the archive's", rel, fi.ModTime())
		}
	}
	check("bin/jq", "binary", 0o555)
	check("COPYING", "license", 0o444)
	if target, err := os.Readlink(filepath.Join(keg, "lib/libjq.dylib")); err != nil || target != "libjq.1.dylib" {
		t.Errorf("symlink = %q, %v", target, err)
	}
	a, _ := os.Stat(filepath.Join(keg, "lib/libjq.1.dylib"))
	b, _ := os.Stat(filepath.Join(keg, "lib/libjq.copy"))
	if !os.SameFile(a, b) {
		t.Error("the hard link was extracted as a copy")
	}
	if _, err := os.Readlink(filepath.Join(keg, "etc-cert")); err != nil {
		t.Errorf("a symlink into the prefix was refused: %v", err)
	}
}

func TestExtractAppliesDirectoryModesLast(t *testing.T) {
	root, err := extract(t,
		dir("jq/1.8.2", 0o755),
		dir("jq/1.8.2/share", 0o555), // read only, with contents after it
		file("jq/1.8.2/share/doc", "d", 0o444),
		dir("jq/1.8.2/share/sub", 0o555),
		file("jq/1.8.2/share/sub/f", "f", 0o444),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(root, stage, "jq/1.8.2/share"), 0o755)
		_ = os.Chmod(filepath.Join(root, stage, "jq/1.8.2/share/sub"), 0o755)
	})
	for _, p := range []string{"share", "share/sub"} {
		fi, err := os.Stat(filepath.Join(root, stage, "jq/1.8.2", p))
		if err != nil || fi.Mode().Perm() != 0o555 || !fi.ModTime().Equal(epoch) {
			t.Errorf("%s = %v %v, %v; want 0555 and the archive's time", p, fi.Mode().Perm(), fi.ModTime(), err)
		}
	}
}

func TestExtractRejects(t *testing.T) {
	tests := map[string][]entry{
		"absolute path":                    {file("/etc/passwd", "x", 0o644)},
		"parent directory":                 {dir("jq/1.8.2", 0o755), file("jq/1.8.2/../../evil", "x", 0o644)},
		"another formula":                  {file("other/1.8.2/f", "x", 0o644)},
		"another version":                  {file("jq/9.9/f", "x", 0o644)},
		"a second keg beside the real one": {dir("jq/1.8.2", 0o755), file("jq/1.8.2/f", "x", 0o644), file("other/1/f", "x", 0o644)},
		"a file beside the keg":            {dir("jq/1.8.2", 0o755), file("jq/README", "x", 0o644)},
		"files at the top":                 {file("README", "x", 0o644)},
		"device":                           {dir("jq/1.8.2", 0o755), {name: "jq/1.8.2/dev", typ: tar.TypeChar, mode: 0o644}},
		"fifo":                             {dir("jq/1.8.2", 0o755), {name: "jq/1.8.2/pipe", typ: tar.TypeFifo, mode: 0o644}},
		"setuid":                           {dir("jq/1.8.2", 0o755), file("jq/1.8.2/su", "x", 0o4755)},
		"setgid":                           {dir("jq/1.8.2", 0o755), file("jq/1.8.2/sg", "x", 0o2755)},
		"sticky":                           {dir("jq/1.8.2", 0o755), dir("jq/1.8.2/tmp", 0o1777)},
		"absolute symlink":                 {dir("jq/1.8.2", 0o755), symlink("jq/1.8.2/x", "/etc/passwd")},
		"symlink out of the prefix":        {dir("jq/1.8.2", 0o755), symlink("jq/1.8.2/x", "../../../../outside")},
		"hard link to a missing file":      {dir("jq/1.8.2", 0o755), hardlink("jq/1.8.2/x", "jq/1.8.2/nothing")},
		"hard link out of the keg":         {dir("jq/1.8.2", 0o755), file("jq/1.8.2/f", "x", 0o644), hardlink("jq/1.8.2/x", "jq/1.8.2/../../f")},
		"hard link to a symlink":           {dir("jq/1.8.2", 0o755), symlink("jq/1.8.2/s", "f"), hardlink("jq/1.8.2/x", "jq/1.8.2/s")},
		"hard link to a directory":         {dir("jq/1.8.2", 0o755), dir("jq/1.8.2/d", 0o755), hardlink("jq/1.8.2/x", "jq/1.8.2/d")},
		"the same file twice":              {dir("jq/1.8.2", 0o755), file("jq/1.8.2/f", "a", 0o644), file("jq/1.8.2/f", "b", 0o644)},
		"a file over a directory":          {dir("jq/1.8.2", 0o755), dir("jq/1.8.2/d", 0o755), file("jq/1.8.2/d", "x", 0o644)},
		"empty name":                       {file("", "x", 0o644)},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			root, err := extract(t, entries...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if _, statErr := os.Lstat(filepath.Join(root, stage)); !errors.Is(statErr, fs.ErrNotExist) {
				t.Errorf("staging was left behind after a failure: %v", statErr)
			}
			if _, statErr := os.Lstat(filepath.Join(root, "outside")); statErr == nil {
				t.Error("something was written outside the keg")
			}
		})
	}
}

func TestExtractNeverWritesThroughASymlink(t *testing.T) {
	tests := map[string]struct {
		entries []entry
		landed  string // where a write through the link would end up, relative to the prefix
	}{
		"a link to a directory of the keg": {
			entries: []entry{
				dir("jq/1.8.2", 0o755), dir("jq/1.8.2/lib", 0o755),
				symlink("jq/1.8.2/alias", "lib"),
				file("jq/1.8.2/alias/evil", "payload", 0o755),
			},
			landed: stage + "/jq/1.8.2/lib/evil",
		},
		"a link to the prefix, which the final location allows": {
			entries: []entry{
				dir("jq/1.8.2", 0o755),
				symlink("jq/1.8.2/up", "../../.."),
				file("jq/1.8.2/up/evil", "payload", 0o755),
			},
			landed: "Cellar/.brw-staging/evil",
		},
		"a directory entry behind a link": {
			entries: []entry{
				dir("jq/1.8.2", 0o755), dir("jq/1.8.2/lib", 0o755),
				symlink("jq/1.8.2/alias", "lib"),
				dir("jq/1.8.2/alias/sub", 0o755),
			},
			landed: stage + "/jq/1.8.2/lib/sub",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			root, err := extract(t, tt.entries...)
			if err == nil {
				t.Fatal("expected an error")
			}
			if _, statErr := os.Lstat(filepath.Join(root, tt.landed)); statErr == nil {
				t.Errorf("an entry was written through the symlink to %s", tt.landed)
			}
		})
	}
}

func TestExtractFailsOnBadArchives(t *testing.T) {
	r, dirPath := prefix(t)
	good := tarball(t, dir("jq/1.8.2", 0o755), file("jq/1.8.2/f", strings.Repeat("x", 10000), 0o644))
	data, _ := os.ReadFile(good)
	truncated := filepath.Join(t.TempDir(), "t.tar.gz")
	if err := os.WriteFile(truncated, data[:len(data)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	notGzip := filepath.Join(t.TempDir(), "n.tar.gz")
	if err := os.WriteFile(notGzip, []byte("plain text"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"truncated": truncated, "not gzip": notGzip, "missing": filepath.Join(t.TempDir(), "none")} {
		if err := Extract(r, path, stage, "jq", "1.8.2"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
		if _, err := os.Lstat(filepath.Join(dirPath, stage)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: staging left behind: %v", name, err)
		}
	}
}

func TestExtractRefusesAnExistingStagingDirectory(t *testing.T) {
	r, dirPath := prefix(t)
	if err := os.MkdirAll(filepath.Join(dirPath, stage), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Extract(r, tarball(t, dir("jq/1.8.2", 0o755)), stage, "jq", "1.8.2"); !errors.Is(err, fsx.ErrExists) {
		t.Errorf("error = %v, want ErrExists", err)
	}
	if _, err := os.Stat(filepath.Join(dirPath, stage)); err != nil {
		t.Errorf("someone else's directory was removed: %v", err)
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"jq/1.8.2/bin/jq": "jq/1.8.2/bin/jq",
		"jq/1.8.2/":       "jq/1.8.2", // a directory entry may end in a slash
	} {
		if got, err := cleanName(in); err != nil || got != want {
			t.Errorf("cleanName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "/", ".", "..", "/etc/passwd", "a/../b", "../a", "a//b", "./a", "a/./b", "a\x00b", "jq/1.8.2/\x00"} {
		if got, err := cleanName(in); err == nil {
			t.Errorf("cleanName(%q) = %q, want an error", in, got)
		}
	}
}
