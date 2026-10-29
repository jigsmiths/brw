package fsx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func newRoot(t *testing.T) (*Root, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	var journal bytes.Buffer
	r, err := Open(dir, &journal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return r, dir, &journal
}

func TestOpenMissing(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Error("expected error")
	}
}

func TestWriteFile(t *testing.T) {
	r, dir, _ := newRoot(t)
	if err := r.WriteFile("a/b/c.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "a/b/c.txt"))
	if err != nil || string(got) != "hi" {
		t.Fatalf("content = %q, %v", got, err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "a/b/c.txt")); fi.Mode().Perm() != 0o644 {
		t.Errorf("perm = %v", fi.Mode().Perm())
	}
}

func TestWriteFileNeverClobbers(t *testing.T) {
	r, dir, _ := newRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := r.WriteFile("x", []byte("new"), 0o644)
	if !errors.Is(err, ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "x")); string(got) != "orig" {
		t.Errorf("file was modified: %q", got)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("leftover temp files: %v", entries)
	}
}

func TestPathEscapesRejected(t *testing.T) {
	r, dir, _ := newRoot(t)
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../x", "a/../../x", "evil/x", filepath.Join(outside, "abs")} {
		if err := r.WriteFile(p, []byte("x"), 0o644); err == nil {
			t.Errorf("WriteFile(%q) succeeded", p)
		}
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Errorf("wrote outside the root: %v", entries)
	}
}

func TestSymlink(t *testing.T) {
	r, dir, _ := newRoot(t)
	if err := r.Symlink("../Cellar/jq/1.0", "opt/jq"); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(dir, "opt/jq"))
	if err != nil || got != "../Cellar/jq/1.0" {
		t.Errorf("target = %q, %v", got, err)
	}
	if err := r.Symlink("../Cellar/jq/2.0", "opt/jq"); !errors.Is(err, ErrExists) {
		t.Errorf("replace error = %v, want ErrExists", err)
	}
	if got, _ := os.Readlink(filepath.Join(dir, "opt/jq")); got != "../Cellar/jq/1.0" {
		t.Errorf("existing link changed to %q", got)
	}
}

func TestSymlinkTargetMustStayInside(t *testing.T) {
	r, dir, _ := newRoot(t)
	if err := r.Symlink("../../../etc/passwd", "bin/x"); err == nil {
		t.Error("relative escape accepted")
	}
	if err := r.Symlink("/etc/passwd", "bin/y"); err == nil {
		t.Error("absolute outside target accepted")
	}
	if err := r.Symlink(filepath.Join(dir, "Caskroom/app/bin/app"), "bin/app"); err != nil {
		t.Errorf("absolute target inside root rejected: %v", err)
	}
}

func TestRemove(t *testing.T) {
	r, dir, _ := newRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("link"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("remove followed the symlink: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "d/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Remove("d"); err == nil {
		t.Error("removed a non-empty directory")
	}
}

func TestRemoveAll(t *testing.T) {
	r, dir, _ := newRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "Cellar/jq/1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "Cellar/jq/1.0/out")); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveAll("Cellar/jq"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Cellar/jq")); !os.IsNotExist(err) {
		t.Errorf("tree still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("RemoveAll followed a symlink out of the root: %v", err)
	}
	for _, p := range []string{"", ".", "./", "a/.."} {
		if err := r.RemoveAll(p); !errors.Is(err, ErrRoot) {
			t.Errorf("RemoveAll(%q) error = %v, want ErrRoot", p, err)
		}
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("root was removed: %v", err)
	}
}

func TestJournal(t *testing.T) {
	r, _, journal := newRoot(t)
	steps := []error{
		r.WriteFile("Cellar/x/1.0/INSTALL_RECEIPT.json", []byte("{}"), 0o644),
		r.Symlink("../Cellar/x/1.0", "opt/x"),
		r.WriteFile("Cellar/x/1.0/INSTALL_RECEIPT.json", []byte("{}"), 0o644), // fails: exists
		r.Remove("opt/x"),
		r.RemoveAll("Cellar/x"),
	}
	if steps[2] == nil {
		t.Fatal("duplicate write should fail")
	}
	got := strings.Split(strings.TrimSpace(journal.String()), "\n")
	want := []string{
		"write Cellar/x/1.0/INSTALL_RECEIPT.json",
		"symlink opt/x -> ../Cellar/x/1.0",
		"remove opt/x",
		"removeall Cellar/x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("journal =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestOpenLock(t *testing.T) {
	r, dir, _ := newRoot(t)
	f, err := r.OpenLock("var/homebrew/locks/jq.formula.lock")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "var/homebrew/locks/jq.formula.lock")); err != nil {
		t.Fatal(err)
	}
	g, err := r.OpenLock("var/homebrew/locks/jq.formula.lock")
	if err != nil {
		t.Fatalf("reopening an existing lock file: %v", err)
	}
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenLockRefusesSymlinkAndEscape(t *testing.T) {
	r, dir, _ := newRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "real"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link.lock")); err != nil {
		t.Fatal(err)
	}
	if f, err := r.OpenLock("link.lock"); err == nil {
		_ = f.Close()
		t.Error("followed a symlink")
	}
	if f, err := r.OpenLock("../outside.lock"); err == nil {
		_ = f.Close()
		t.Error("escaped the root")
	}
}

func TestLstat(t *testing.T) {
	r, dir, _ := newRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := r.Lstat("x")
	if err != nil || fi.Size() != 3 {
		t.Errorf("Lstat = %v, %v", fi, err)
	}
	if _, err := r.Lstat("../x"); err == nil {
		t.Error("Lstat escaped the root")
	}
}

func TestMkdir(t *testing.T) {
	r, dir, journal := newRoot(t)
	if err := r.Mkdir("a", 0o755); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "a")); err != nil || !fi.IsDir() {
		t.Fatalf("a was not created: %v", err)
	}
	if err := r.Mkdir("a", 0o755); !errors.Is(err, ErrExists) {
		t.Errorf("second Mkdir error = %v, want ErrExists", err)
	}
	if err := r.Mkdir("x/y", 0o755); err == nil {
		t.Error("Mkdir created a directory under a missing parent")
	}
	for _, p := range []string{"../out", "a/../../out"} {
		if err := r.Mkdir(p, 0o755); err == nil {
			t.Errorf("Mkdir(%q) escaped the root", p)
		}
	}
	if got := journal.String(); got != "mkdir a\n" {
		t.Errorf("journal = %q", got)
	}
}

func TestMkdirAll(t *testing.T) {
	r, dir, journal := newRoot(t)
	if err := os.Mkdir(filepath.Join(dir, "Cellar"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.MkdirAll("Cellar/jq/1.0/bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.MkdirAll("Cellar/jq/1.0/bin", 0o755); err != nil {
		t.Errorf("MkdirAll on an existing directory failed: %v", err)
	}
	want := "mkdir Cellar/jq\nmkdir Cellar/jq/1.0\nmkdir Cellar/jq/1.0/bin\n"
	if got := journal.String(); got != want {
		t.Errorf("journal = %q, want only the directories it created: %q", got, want)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.MkdirAll("file/sub", 0o755); err == nil {
		t.Error("MkdirAll went through a regular file")
	}
	if err := r.MkdirAll("file", 0o755); err == nil {
		t.Error("MkdirAll reported success for a path that is a regular file")
	}
	if err := r.MkdirAll("../out", 0o755); err == nil {
		t.Error("MkdirAll escaped the root")
	}
}

func TestChmod(t *testing.T) {
	r, dir, journal := newRoot(t)
	if err := r.WriteFile("bin/tool", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal.Reset()
	if err := r.Chmod("bin/tool", 0o555); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "bin/tool")); fi.Mode().Perm() != 0o555 {
		t.Errorf("perm = %v", fi.Mode().Perm())
	}
	if got := journal.String(); got != "chmod bin/tool 555\n" {
		t.Errorf("journal = %q", got)
	}
	if err := r.Chmod("../out", 0o755); err == nil {
		t.Error("Chmod escaped the root")
	}
}

func TestLink(t *testing.T) {
	r, dir, journal := newRoot(t)
	if err := r.WriteFile("a/one", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal.Reset()
	if err := r.Link("a/one", "a/two"); err != nil {
		t.Fatal(err)
	}
	one, _ := os.Stat(filepath.Join(dir, "a/one"))
	two, _ := os.Stat(filepath.Join(dir, "a/two"))
	if !os.SameFile(one, two) {
		t.Error("the two names are not the same file")
	}
	if err := r.Link("a/one", "a/two"); !errors.Is(err, ErrExists) {
		t.Errorf("second Link error = %v, want ErrExists", err)
	}
	if got := journal.String(); got != "link a/one a/two\n" {
		t.Errorf("journal = %q", got)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Link(outside, "a/leak"); err == nil {
		t.Error("Link took a file from outside the root")
	}
	if err := r.Link("a/one", "../out"); err == nil {
		t.Error("Link wrote outside the root")
	}
}

func TestRename(t *testing.T) {
	r, dir, journal := newRoot(t)
	if err := r.WriteFile("stage/jq/1.0/bin/jq", []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Rename("stage/jq/1.0", "Cellar/jq-1.0"); err == nil {
		t.Fatal("Rename created the missing parent")
	}
	if err := r.Mkdir("Cellar", 0o755); err != nil {
		t.Fatal(err)
	}
	journal.Reset()
	if err := r.Rename("stage/jq/1.0", "Cellar/jq-1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "Cellar/jq-1.0/bin/jq")); err != nil {
		t.Errorf("directory did not move: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "stage/jq/1.0")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("source still exists: %v", err)
	}
	if got := journal.String(); got != "rename stage/jq/1.0 Cellar/jq-1.0\n" {
		t.Errorf("journal = %q", got)
	}
}

func TestRenameNeverReplaces(t *testing.T) {
	r, dir, _ := newRoot(t)
	for _, f := range []string{"new/f", "old/g"} {
		if err := r.WriteFile(f, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// An existing empty directory would be replaced by a plain rename.
	if err := r.Mkdir("empty", 0o755); err != nil {
		t.Fatal(err)
	}
	for _, dst := range []string{"old", "empty", "old/g"} {
		if err := r.Rename("new", dst); !errors.Is(err, ErrExists) {
			t.Errorf("Rename onto %s: error = %v, want ErrExists", dst, err)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "new/f")); string(got) != "new/f" {
		t.Error("source was disturbed")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "old/g")); string(got) != "old/g" {
		t.Error("destination was disturbed")
	}
	if err := r.Rename("missing", "elsewhere"); err == nil {
		t.Error("renaming a missing path succeeded")
	}
	outside := t.TempDir()
	if err := r.Rename("new", filepath.Join(outside, "x")); err == nil {
		t.Error("Rename moved a path out of the root")
	}
	if err := r.Rename("../x", "y"); err == nil {
		t.Error("Rename moved a path in from outside the root")
	}
}

// slowReader returns n bytes of a pattern in small chunks, then optionally an error.
type slowReader struct {
	n    int
	fail error
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.n == 0 {
		if s.fail != nil {
			return 0, s.fail
		}
		return 0, io.EOF
	}
	n := min(len(p), s.n, 4096)
	for i := range n {
		p[i] = byte(s.n - i)
	}
	s.n -= n
	return n, nil
}

func TestWriteFromStreams(t *testing.T) {
	r, dir, journal := newRoot(t)
	const size = 5 << 20
	if err := r.Mkdir("big", 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Date(2026, 6, 20, 15, 36, 55, 0, time.UTC)
	if err := r.WriteFrom("big/file", &slowReader{n: size}, 0o555, when); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "big/file"))
	if err != nil || fi.Size() != size || fi.Mode().Perm() != 0o555 || !fi.ModTime().Equal(when) {
		t.Fatalf("size = %d, mode = %v, mtime = %v, %v", fi.Size(), fi.Mode().Perm(), fi.ModTime(), err)
	}
	if err := r.WriteFrom("big/now", strings.NewReader("x"), 0o644, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "big/now")); time.Since(fi.ModTime()) > time.Minute {
		t.Errorf("a zero time should leave the current time, got %v", fi.ModTime())
	}
	if err := r.WriteFrom("missing/file", strings.NewReader("x"), 0o644, time.Time{}); err == nil {
		t.Error("WriteFrom created a missing parent")
	}
	if got := journal.String(); got != "mkdir big\nwrite big/file\nwrite big/now\n" {
		t.Errorf("journal = %q", got)
	}
}

func TestWriteFromNeverClobbers(t *testing.T) {
	r, dir, _ := newRoot(t)
	if err := os.WriteFile(filepath.Join(dir, "x"), []byte("orig"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFrom("x", strings.NewReader("new"), 0o644, time.Time{}); !errors.Is(err, ErrExists) {
		t.Fatalf("error = %v, want ErrExists", err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "x")); string(got) != "orig" {
		t.Errorf("file was modified: %q", got)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("leftover temp files: %v", entries)
	}
}

func TestWriteFromFailureLeavesNothing(t *testing.T) {
	r, dir, journal := newRoot(t)
	boom := errors.New("connection reset")
	if err := r.Mkdir("a", 0o755); err != nil {
		t.Fatal(err)
	}
	journal.Reset()
	err := r.WriteFrom("a/file", &slowReader{n: 10000, fail: boom}, 0o644, time.Time{})
	if !errors.Is(err, boom) {
		t.Fatalf("error = %v, want the reader's", err)
	}
	var left []string
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			left = append(left, p)
		}
		return nil
	})
	if len(left) != 0 {
		t.Errorf("files left behind: %v", left)
	}
	if journal.Len() != 0 {
		t.Errorf("journal = %q, want nothing for a failed write", journal.String())
	}
}

func TestChtimes(t *testing.T) {
	r, dir, journal := newRoot(t)
	if err := r.WriteFile("f", []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal.Reset()
	when := time.Date(2026, 6, 20, 15, 36, 55, 0, time.UTC)
	if err := r.Chtimes("f", when, when); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "f")); !fi.ModTime().Equal(when) {
		t.Errorf("mtime = %v, want %v", fi.ModTime(), when)
	}
	if got := journal.String(); got != "chtimes f\n" {
		t.Errorf("journal = %q", got)
	}
	if err := r.Chtimes("../out", when, when); err == nil {
		t.Error("Chtimes escaped the root")
	}
	if err := r.Chtimes("missing", when, when); err == nil {
		t.Error("Chtimes on a missing file succeeded")
	}
}

func TestReplaceInStaging(t *testing.T) {
	r, dir, journal := newRoot(t)
	one, two := Staging+"/id/jq/1.0/lib/libjq.dylib", Staging+"/id/jq/1.0/lib/libjq.1.dylib"
	if err := r.WriteFile(one, []byte("@@HOMEBREW_PREFIX@@/lib"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := r.Link(one, two); err != nil {
		t.Fatal(err)
	}
	journal.Reset()
	if err := r.Replace(one, []byte("/opt/brw/lib")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{one, two} {
		got, err := os.ReadFile(filepath.Join(dir, p))
		if err != nil || string(got) != "/opt/brw/lib" {
			t.Errorf("%s = %q, %v; both names of a hard linked file must change", p, got, err)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, one)); fi.Mode().Perm() != 0o555 {
		t.Errorf("perm = %v, want the read only mode back", fi.Mode().Perm())
	}
	if got := journal.String(); got != "replace "+one+"\n" {
		t.Errorf("journal = %q", got)
	}
}

func TestReplaceRefusesAnythingElse(t *testing.T) {
	r, dir, _ := newRoot(t)
	if err := r.WriteFile("Cellar/jq/1.0/bin/jq", []byte("live"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace("Cellar/jq/1.0/bin/jq", []byte("changed")); err == nil {
		t.Error("Replace rewrote a file outside the staging area")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "Cellar/jq/1.0/bin/jq")); string(got) != "live" {
		t.Errorf("live file changed: %q", got)
	}
	if err := r.Replace(Staging+"/../jq/1.0/bin/jq", []byte("changed")); err == nil {
		t.Error("a path that climbs out of staging was accepted")
	}

	if err := r.Mkdir(Staging, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace(Staging+"/missing", nil); err == nil {
		t.Error("Replace created a file")
	}
	if err := r.Mkdir(Staging+"/d", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace(Staging+"/d", nil); err == nil {
		t.Error("Replace accepted a directory")
	}
	if err := r.Symlink("../../Cellar/jq/1.0/bin/jq", Staging+"/link"); err != nil {
		t.Fatal(err)
	}
	if err := r.Replace(Staging+"/link", []byte("changed")); err == nil {
		t.Error("Replace followed a symlink")
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "Cellar/jq/1.0/bin/jq")); string(got) != "live" {
		t.Errorf("a file behind a symlink changed: %q", got)
	}
}

func TestReadFile(t *testing.T) {
	r, dir, journal := newRoot(t)
	if err := r.WriteFile("a/f", []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	journal.Reset()
	got, err := r.ReadFile("a/f")
	if err != nil || string(got) != "data" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if journal.Len() != 0 {
		t.Errorf("reading was journaled: %q", journal.String())
	}
	if _, err := r.ReadFile(filepath.Join(dir, "a/f")); err != nil {
		t.Errorf("absolute path inside the root: %v", err)
	}
	if _, err := r.ReadFile("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing file error = %v", err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../secret", outside, "evil"} {
		if got, err := r.ReadFile(p); err == nil {
			t.Errorf("ReadFile(%q) read outside the root: %q", p, got)
		}
	}
}

func TestAbs(t *testing.T) {
	r, dir, _ := newRoot(t)
	got, err := r.Abs("Cellar/jq/1.0")
	if err != nil || got != filepath.Join(dir, "Cellar/jq/1.0") {
		t.Errorf("Abs = %q, %v", got, err)
	}
	if got, err := r.Abs(filepath.Join(dir, "x")); err != nil || got != filepath.Join(dir, "x") {
		t.Errorf("Abs of an absolute path inside = %q, %v", got, err)
	}
	for _, p := range []string{"../out", filepath.Join(t.TempDir(), "elsewhere")} {
		if got, err := r.Abs(p); err == nil {
			t.Errorf("Abs(%q) = %q, want an error", p, got)
		}
	}
}

func TestReadlinkAndReadDir(t *testing.T) {
	r, dir, journal := newRoot(t)
	if err := r.WriteFile("d/b", []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile("d/a", []byte("2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Symlink("../d/a", "d/link"); err != nil {
		t.Fatal(err)
	}
	journal.Reset()

	if got, err := r.Readlink("d/link"); err != nil || got != "../d/a" {
		t.Errorf("Readlink = %q, %v", got, err)
	}
	if _, err := r.Readlink("d/a"); err == nil {
		t.Error("Readlink of a regular file succeeded")
	}
	entries, err := r.ReadDir("d")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if !reflect.DeepEqual(names, []string{"a", "b", "link"}) {
		t.Errorf("ReadDir = %v, want the names in order", names)
	}
	if entries[2].Type()&os.ModeSymlink == 0 {
		t.Error("ReadDir did not report the symlink as one")
	}
	if journal.Len() != 0 {
		t.Errorf("reading was journaled: %q", journal.String())
	}
	if _, err := r.ReadDir("missing"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("missing directory: %v", err)
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, "evil")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../x", outside, "evil"} {
		if _, err := r.ReadDir(p); err == nil {
			t.Errorf("ReadDir(%q) read outside the root", p)
		}
	}
	if _, err := r.Readlink("../x"); err == nil {
		t.Error("Readlink escaped the root")
	}
}

func TestRemoveKeg(t *testing.T) {
	r, dir, journal := newRoot(t)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "keep"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(dir, "Cellar/jq/1.0")
	for _, p := range []string{"bin", "share/doc/jq", "share/man"} {
		if err := os.MkdirAll(filepath.Join(keg, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(keg, "bin/jq"), []byte("x"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keg, "share/doc/jq/README"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(keg, "out")); err != nil {
		t.Fatal(err)
	}
	// Read only directories, as bottles ship them; the deepest first.
	for _, p := range []string{"share/doc/jq", "share/doc", "share/man", "share", "bin"} {
		if err := os.Chmod(filepath.Join(keg, p), 0o555); err != nil {
			t.Fatal(err)
		}
	}
	other := filepath.Join(dir, "Cellar/jq/2.0")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := r.RemoveKeg("Cellar/jq/1.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(keg); !os.IsNotExist(err) {
		t.Errorf("keg still exists: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another version was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Errorf("RemoveKeg followed a symlink out of the keg: %v", err)
	}
	if !strings.Contains(journal.String(), "removekeg Cellar/jq/1.0\n") {
		t.Errorf("journal = %q", journal.String())
	}
}

func TestRemoveKegRefusesAnythingElse(t *testing.T) {
	r, dir, _ := newRoot(t)
	for _, p := range []string{"Cellar/jq/1.0/bin", "Cellar/.brw-staging/x", "Cellar/.hidden/1.0", "bin/tool/1.0", "etc/jq/1.0", "var/Cellar/jq"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Join(dir, "etc"), filepath.Join(dir, "Cellar/jq/2.0")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Cellar/jq/3.0"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		"", ".", "Cellar", "Cellar/jq", "Cellar/jq/1.0/bin", "Cellar/.brw-staging/x", "Cellar/.hidden/1.0",
		"bin/tool/1.0", "etc/jq/1.0", "Cellar/jq/2.0", "Cellar/jq/3.0", "Cellar/nothing/1.0", "../Cellar/jq/1.0", "Cellar/jq/../jq/1.0/..",
	} {
		if err := r.RemoveKeg(p); err == nil {
			t.Errorf("RemoveKeg(%q) succeeded", p)
		}
	}
	for _, p := range []string{"Cellar/jq/1.0/bin", "Cellar/.brw-staging/x", "etc", "Cellar/jq/3.0"} {
		if _, err := os.Lstat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s was damaged: %v", p, err)
		}
	}
}

// snapshotTree lists everything below dir with its mode, size and time.
func snapshotTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		line := fmt.Sprintf("%s %v %d %d", rel, fi.Mode(), fi.Size(), fi.ModTime().UnixNano())
		if fi.Mode()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			line += " -> " + target
		}
		out = append(out, line)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestDryRunChangesNothingAndSaysWhatItWould(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"Cellar/jq/1.0/bin", "Cellar/.brw-staging/x", "etc", "opt"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for p, mode := range map[string]os.FileMode{"file": 0o644, "etc/conf": 0o644, "Cellar/jq/1.0/bin/jq": 0o555, "Cellar/.brw-staging/x/f": 0o644} {
		if err := os.WriteFile(filepath.Join(dir, p), []byte("data"), mode); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("file", filepath.Join(dir, "opt/link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "Cellar/jq/1.0/bin"), 0o555); err != nil { // read only, as bottles ship them
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "Cellar/jq/1.0/bin"), 0o755) })

	var journal bytes.Buffer
	r, err := OpenDryRun(dir, &journal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	before := snapshotTree(t, dir)

	later := time.Now().Add(time.Hour)
	for name, fn := range map[string]func() error{
		"write":         func() error { return r.WriteFile("new/dir/f", []byte("x"), 0o644) },
		"writefrom":     func() error { return r.WriteFrom("Cellar/.brw-staging/x/g", strings.NewReader("x"), 0o644, later) },
		"symlink":       func() error { return r.Symlink("../file", "bin/new/link") },
		"mkdir":         func() error { return r.Mkdir("fresh", 0o755) },
		"mkdirall":      func() error { return r.MkdirAll("a/b/c", 0o755) },
		"chtimes":       func() error { return r.Chtimes("file", later, later) },
		"chmod":         func() error { return r.Chmod("file", 0o600) },
		"link":          func() error { return r.Link("file", "file2") },
		"rename":        func() error { return r.Rename("file", "moved") },
		"replace":       func() error { return r.Replace("Cellar/.brw-staging/x/f", []byte("changed")) },
		"remove":        func() error { return r.Remove("file") },
		"removeall":     func() error { return r.RemoveAll("etc") },
		"removekeg":     func() error { return r.RemoveKeg("Cellar/jq/1.0") },
		"removestaging": func() error { return r.RemoveStaging("Cellar/.brw-staging/x") },
	} {
		if err := fn(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if after := snapshotTree(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("a dry run changed the tree\nbefore: %v\nafter:  %v", before, after)
	}
	lines := strings.Split(strings.TrimSpace(journal.String()), "\n")
	for _, want := range []string{
		"would write new/dir/f", "would write Cellar/.brw-staging/x/g", "would symlink bin/new/link -> ../file", "would mkdir fresh",
		"would mkdir a", "would mkdir a/b", "would mkdir a/b/c", "would chtimes file", "would chmod file 600", "would link file file2",
		"would rename file moved", "would replace Cellar/.brw-staging/x/f", "would remove file", "would removeall etc", "would removekeg Cellar/jq/1.0", "would removestaging Cellar/.brw-staging/x",
	} {
		if !slices.Contains(lines, want) {
			t.Errorf("the journal lacks %q:\n%s", want, journal.String())
		}
	}
	for _, l := range lines {
		if !strings.HasPrefix(l, "would ") {
			t.Errorf("a journal line does not say it is hypothetical: %q", l)
		}
	}
}

func TestDryRunStillRefusesWhatARealRunWould(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenDryRun(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	for name, fn := range map[string]func() error{
		"escape":         func() error { return r.Remove("../outside") },
		"absolute":       func() error { return r.Symlink("x", "/etc/passwd") },
		"symlink target": func() error { return r.Symlink("../../out", "link") },
		"root":           func() error { return r.RemoveAll(".") },
		"not a keg":      func() error { return r.RemoveKeg("Cellar/jq") },
		"outside staging": func() error {
			return r.Replace("file", nil)
		},
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: a dry run accepted what a real one refuses", name)
		}
	}
	if _, err := r.OpenLock("var/homebrew/locks/jq.formula.lock"); err == nil {
		t.Error("a dry run must not create a lock file")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the dry run wrote %v", entries)
	}
}

func TestDryRunReportsWhatAlreadyExistsLikeARealRun(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []string{"file", "link"} {
		if p == "file" {
			if err := os.WriteFile(filepath.Join(dir, p), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		} else if err := os.Symlink("nowhere", filepath.Join(dir, p)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(dir, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := OpenDryRun(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	for name, fn := range map[string]func() error{
		"write file":            func() error { return r.WriteFile("file", nil, 0o644) },
		"write dangling link":   func() error { return r.WriteFile("link", nil, 0o644) },
		"writefrom":             func() error { return r.WriteFrom("file", strings.NewReader(""), 0o644, time.Time{}) },
		"symlink onto file":     func() error { return r.Symlink("x", "file") },
		"symlink onto link":     func() error { return r.Symlink("x", "link") },
		"symlink onto dir":      func() error { return r.Symlink("x", "dir") },
		"mkdir onto dir":        func() error { return r.Mkdir("dir", 0o755) },
		"mkdir onto file":       func() error { return r.Mkdir("file", 0o755) },
		"link onto file":        func() error { return r.Link("file", "dir") },
		"mkdirall through file": func() error { return r.MkdirAll("file/sub", 0o755) },
	} {
		if err := fn(); err == nil {
			t.Errorf("%s: a dry run accepted what a real run reports as existing", name)
		} else if name != "mkdirall through file" && !errors.Is(err, ErrExists) {
			t.Errorf("%s: err = %v, want ErrExists", name, err)
		}
	}
}

func TestDryRunReportsWhatIsMissingLikeARealRun(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	r, err := OpenDryRun(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	now := time.Now()
	for name, fn := range map[string]func() error{
		"remove":           func() error { return r.Remove("missing") },
		"chmod":            func() error { return r.Chmod("missing", 0o600) },
		"chtimes":          func() error { return r.Chtimes("missing", now, now) },
		"rename":           func() error { return r.Rename("missing", "other") },
		"link":             func() error { return r.Link("missing", "other") },
		"mkdir no parent":  func() error { return r.Mkdir("a/b", 0o755) },
		"writefrom parent": func() error { return r.WriteFrom("a/f", strings.NewReader(""), 0o644, time.Time{}) },
	} {
		if err := fn(); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s: err = %v, want not exist", name, err)
		}
	}
	if err := r.RemoveAll("missing"); err != nil {
		t.Errorf("RemoveAll of nothing is fine in a real run: %v", err)
	}
}

func TestRemoveStaging(t *testing.T) {
	r, dir, journal := newRoot(t)
	stage := filepath.Join(dir, Staging, "abc", "jq", "1.0", "bin")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "jq"), []byte("x"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stage, 0o555); err != nil { // read only, as an unpacked keg is
		t.Fatal(err)
	}
	other := filepath.Join(dir, Staging, "keep")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveStaging(Staging + "/abc"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, Staging, "abc")); !os.IsNotExist(err) {
		t.Errorf("the staging directory stayed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("another staging directory went: %v", err)
	}
	if !strings.Contains(journal.String(), "removestaging "+Staging+"/abc\n") {
		t.Errorf("journal = %q", journal.String())
	}
}

func TestRemoveStagingRefusesAnythingElse(t *testing.T) {
	r, dir, _ := newRoot(t)
	for _, p := range []string{"Cellar/jq/1.0", Staging + "/x", "etc"} {
		if err := os.MkdirAll(filepath.Join(dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(dir, Staging, "link")); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"", ".", "Cellar", Staging, "Cellar/jq/1.0", "etc", "../x", Staging + "/missing", Staging + "/link"} {
		if err := r.RemoveStaging(p); err == nil {
			t.Errorf("RemoveStaging(%q) succeeded", p)
		}
	}
	for _, p := range []string{"Cellar/jq/1.0", "etc", Staging + "/x"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("%s was damaged: %v", p, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Errorf("the target of a link was damaged: %v", err)
	}
}

func TestDryRunAllowsWhatARealRunCouldDoAfterARemove(t *testing.T) {
	dir := t.TempDir()
	if err := os.Symlink("nowhere", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	r, err := OpenDryRun(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.Remove("link"); err != nil {
		t.Fatal(err)
	}
	if err := r.Symlink("x", "link"); err != nil {
		t.Errorf("symlink where a removed link was: %v", err)
	}
}
