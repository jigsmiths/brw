package spike

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/lock"
)

var now = time.Unix(1782112702, 0)

// newPrefix makes a prefix with brew's standard directories.
func newPrefix(t *testing.T) (string, *fsx.Root) {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"Cellar", "opt", "bin", "var/homebrew/linked", "var/homebrew/locks"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	r, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Error(err)
		}
	})
	return dir, r
}

// snapshot lists every path with its symlink target, ignoring lock files.
func snapshot(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir || strings.HasSuffix(p, ".lock") {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.Type()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			rel += " -> " + target
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCreateKeg(t *testing.T) {
	dir, r := newPrefix(t)
	p := cellar.Prefix(dir)
	if err := CreateKeg(r, "brw-spike-test", "0.0.1", now); err != nil {
		t.Fatal(err)
	}

	kegs, err := p.Kegs()
	if err != nil || len(kegs) != 1 || kegs[0].Name != "brw-spike-test" || kegs[0].Version != "0.0.1" {
		t.Fatalf("Kegs = %+v, %v", kegs, err)
	}
	rec := kegs[0].Receipt
	if rec == nil || !rec.InstalledOnRequest || rec.Time != now.Unix() {
		t.Errorf("receipt = %+v", rec)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "Cellar/brw-spike-test/0.0.1/INSTALL_RECEIPT.json"))
	if out, err := rec.Marshal(); err != nil || string(out) != string(b) {
		t.Errorf("receipt on disk is not in brew's format: %v", err)
	}
	if v, ok := p.OptVersion("brw-spike-test"); !ok || v != "0.0.1" {
		t.Errorf("OptVersion = %q, %v", v, ok)
	}
	if !p.Linked("brw-spike-test", "0.0.1") {
		t.Error("not linked")
	}
	if got, _ := os.Readlink(filepath.Join(dir, "bin/brw-spike-test")); got != "../Cellar/brw-spike-test/0.0.1/bin/brw-spike-test" {
		t.Errorf("bin link = %q", got)
	}
	if fi, err := os.Stat(filepath.Join(dir, "bin/brw-spike-test")); err != nil || fi.Mode()&0o111 == 0 {
		t.Errorf("linked script not executable: %v", err)
	}
}

func TestCreateKegTwiceFails(t *testing.T) {
	dir, r := newPrefix(t)
	if err := CreateKeg(r, "x", "1", now); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	if err := CreateKeg(r, "x", "1", now); !errors.Is(err, fsx.ErrExists) {
		t.Errorf("error = %v, want ErrExists", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, dir)) {
		t.Error("second create changed the prefix")
	}
}

func TestCreateKegNeedsTheLock(t *testing.T) {
	dir, r := newPrefix(t)
	l, err := lock.Acquire(r, "x")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := l.Release(); err != nil {
			t.Error(err)
		}
	})
	before := snapshot(t, dir)
	if err := CreateKeg(r, "x", "1", now); !errors.Is(err, lock.ErrInProgress) {
		t.Errorf("error = %v, want ErrInProgress", err)
	}
	if !reflect.DeepEqual(before, snapshot(t, dir)) {
		t.Error("wrote files without the lock")
	}
}

func TestCreateThenRemoveRestoresTree(t *testing.T) {
	dir, r := newPrefix(t)
	p := cellar.Prefix(dir)
	before := snapshot(t, dir)
	if err := CreateKeg(r, "x", "1", now); err != nil {
		t.Fatal(err)
	}
	if err := RemoveKeg(r, p, "x", "1"); err != nil {
		t.Fatal(err)
	}
	if after := snapshot(t, dir); !reflect.DeepEqual(before, after) {
		t.Errorf("tree changed\nbefore %v\nafter  %v", before, after)
	}
}

func TestRemoveKegLeavesForeignLinks(t *testing.T) {
	dir, r := newPrefix(t)
	p := cellar.Prefix(dir)
	if err := CreateKeg(r, "x", "1", now); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(dir, "bin/x")
	if err := os.Remove(foreign); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/other/9/bin/x", foreign); err != nil {
		t.Fatal(err)
	}
	before := snapshot(t, dir)
	if err := RemoveKeg(r, p, "x", "1"); err == nil {
		t.Fatal("expected an error")
	}
	if !reflect.DeepEqual(before, snapshot(t, dir)) {
		t.Error("RemoveKeg changed the prefix despite the foreign link")
	}
}

func TestRemoveKegMissingLinksIsFine(t *testing.T) {
	dir, r := newPrefix(t)
	p := cellar.Prefix(dir)
	if err := CreateKeg(r, "x", "1", now); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "opt/x")); err != nil {
		t.Fatal(err)
	}
	if err := RemoveKeg(r, p, "x", "1"); err != nil {
		t.Errorf("RemoveKeg = %v", err)
	}
}
