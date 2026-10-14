package pin

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
)

func setup(t *testing.T, versions ...string) (*fsx.Root, string, []cellar.Keg) {
	t.Helper()
	dir := t.TempDir()
	var kegs []cellar.Keg
	for _, v := range versions {
		path := filepath.Join(dir, "Cellar", "app", v)
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		kegs = append(kegs, cellar.Keg{Name: "app", Version: v, Path: path})
	}
	root, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir, kegs
}

func TestPinNewestKeg(t *testing.T) {
	root, dir, kegs := setup(t, "1.9", "1.10", "1.2")
	if err := Pin(root, kegs); err != nil {
		t.Fatal(err)
	}
	got, err := os.Readlink(filepath.Join(dir, "var/homebrew/pinned/app"))
	if err != nil || got != "../../../Cellar/app/1.10" {
		t.Errorf("record -> %q, %v", got, err)
	}
	if !Pinned(dir, "app") {
		t.Error("Pinned says no")
	}
}

func TestPinnedMeansASymlinkEvenWhenItDangles(t *testing.T) {
	root, dir, kegs := setup(t, "1.0")
	if Pinned(dir, "app") {
		t.Fatal("pinned before pinning")
	}
	if err := Pin(root, kegs); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "Cellar/app/1.0")); err != nil {
		t.Fatal(err)
	}
	if !Pinned(dir, "app") {
		t.Error("a pin whose keg is gone still counts, as in brew")
	}
	if Pinned(dir, "other") {
		t.Error("another formula is not pinned")
	}
}

func TestPinWillNotReplaceWhatIsInTheWay(t *testing.T) {
	root, dir, kegs := setup(t, "1.0")
	if err := os.MkdirAll(filepath.Join(dir, "var/homebrew/pinned/app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Pin(root, kegs); !errors.Is(err, fsx.ErrExists) {
		t.Errorf("error = %v, want ErrExists", err)
	}
	if Pinned(dir, "app") {
		t.Error("a directory is not a pin")
	}
}

func TestUnpinRemovesTheRecordAndTheEmptyDirectory(t *testing.T) {
	root, dir, kegs := setup(t, "1.0")
	other := filepath.Join(dir, "Cellar/zed/2.0")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Pin(root, kegs); err != nil {
		t.Fatal(err)
	}
	if err := Pin(root, []cellar.Keg{{Name: "zed", Version: "2.0", Path: other}}); err != nil {
		t.Fatal(err)
	}
	if err := Unpin(root, "app"); err != nil {
		t.Fatal(err)
	}
	if Pinned(dir, "app") || !Pinned(dir, "zed") {
		t.Error("only app should have been unpinned")
	}
	if _, err := os.Stat(filepath.Join(dir, "var/homebrew/pinned")); err != nil {
		t.Errorf("the directory goes only when it is empty: %v", err)
	}
	if err := Unpin(root, "zed"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "var/homebrew/pinned")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the empty directory is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "var/homebrew")); err != nil {
		t.Errorf("var/homebrew must stay: %v", err)
	}
}
