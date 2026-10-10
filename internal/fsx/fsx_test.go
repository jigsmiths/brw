package fsx

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func newRoot(t *testing.T) (*Root, string, *bytes.Buffer) {
	t.Helper()
	dir := t.TempDir()
	var journal bytes.Buffer
	r, err := Open(dir, &journal)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Close() })
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
	f.Close()
	if _, err := os.Stat(filepath.Join(dir, "var/homebrew/locks/jq.formula.lock")); err != nil {
		t.Fatal(err)
	}
	g, err := r.OpenLock("var/homebrew/locks/jq.formula.lock")
	if err != nil {
		t.Fatalf("reopening an existing lock file: %v", err)
	}
	g.Close()
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
		f.Close()
		t.Error("followed a symlink")
	}
	if f, err := r.OpenLock("../outside.lock"); err == nil {
		f.Close()
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
