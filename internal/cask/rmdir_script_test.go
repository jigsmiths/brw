package cask

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const rmdirScriptSHA256 = "cc38cd939a18f6506c0d831ee18858679f9bffe28a938324a8644caf91edf1e3"

func TestEmbeddedRmdirScriptIsPinned(t *testing.T) {
	sum := sha256.Sum256([]byte(rmdirScript))
	if got := hex.EncodeToString(sum[:]); got != rmdirScriptSHA256 {
		t.Errorf("rmdir.sh sha256 = %s, want %s; update it only from brew's cask/utils/rmdir.sh", got, rmdirScriptSHA256)
	}
}

// Run with BRW_CONFORMANCE=1.
func TestConformanceRmdirScriptIsBrews(t *testing.T) {
	if os.Getenv("BRW_CONFORMANCE") == "" {
		t.Skip("set BRW_CONFORMANCE=1 to compare with brew's copy")
	}
	library := os.Getenv("HOMEBREW_LIBRARY")
	if library == "" {
		library = "/opt/homebrew/Library"
	}
	brews, err := os.ReadFile(filepath.Join(library, "Homebrew/cask/utils/rmdir.sh"))
	if err != nil {
		t.Skip("no Homebrew library to compare with")
	}
	if rmdirScript != string(brews) {
		t.Error("the embedded rmdir.sh differs from this machine's brew")
	}
}

func TestEmbeddedRmdirScriptRemovesWhatBrewsDoes(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "rmdir.sh")
	if err := os.WriteFile(script, []byte(rmdirScript), 0o755); err != nil {
		t.Fatal(err)
	}
	mk := func(dirs ...string) {
		for _, d := range dirs {
			if err := os.MkdirAll(filepath.Join(root, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
	touch := func(name string) {
		if err := os.WriteFile(filepath.Join(root, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("empty", "dsstore", "broken", "full", "target", "linked")
	touch("dsstore/.DS_Store")
	touch("full/file")
	if err := os.Symlink(filepath.Join(root, "gone"), filepath.Join(root, "broken/dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "linked/ignored")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "target"), filepath.Join(root, "symlink")); err != nil {
		t.Fatal(err)
	}
	args := []string{script}
	for _, p := range []string{"empty", "dsstore", "broken", "full", "symlink", "missing"} {
		args = append(args, filepath.Join(root, p))
	}
	if out, err := exec.Command("bash", args...).CombinedOutput(); err != nil {
		t.Fatalf("rmdir.sh: %v\n%s", err, out)
	}
	for path, want := range map[string]bool{
		"empty": false, "dsstore": false, "broken": false, "symlink": false,
		"full": true, "full/file": true, "target": true, "linked": true,
	} {
		_, err := os.Lstat(filepath.Join(root, path))
		if got := err == nil; got != want {
			t.Errorf("%s exists = %v, want %v", path, got, want)
		}
	}
}

func TestEmbeddedRmdirScriptQuietlyHandlesAnAllEmptyBatch(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "rmdir.sh")
	if err := os.WriteFile(script, []byte(rmdirScript), 0o755); err != nil {
		t.Fatal(err)
	}
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	for _, d := range []string{a, b} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("bash", script, a, b).CombinedOutput(); err != nil || len(out) != 0 {
		t.Errorf("out = %q, err = %v", out, err)
	}
	if _, err := os.Lstat(a); err == nil {
		t.Error("a should be gone")
	}
}
