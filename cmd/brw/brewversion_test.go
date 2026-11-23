package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A receipt of a keg brw builds records the prefix's Homebrew version, as
// brew's does, since brew trusts runtime_dependencies only from 1.1.6 on.
func TestHomebrewVersionIsThePrefixsBrew(t *testing.T) {
	t.Run("from brew's repository", func(t *testing.T) {
		prefix := t.TempDir()
		gitIn(t, prefix, "init", "-q")
		if err := os.MkdirAll(filepath.Join(prefix, "Library", "Homebrew"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prefix, "Library", "Homebrew", "brew.sh"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitIn(t, prefix, "add", ".")
		gitIn(t, prefix, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-qm", "one")
		gitIn(t, prefix, "tag", "5.0.3")
		if got := homebrewVersion(prefix); got != "5.0.3" {
			t.Errorf("at a tag: %q", got)
		}
		gitIn(t, prefix, "-c", "user.email=t@t", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "two")
		if got := homebrewVersion(prefix); !strings.HasPrefix(got, "5.0.3-1-g") || len(got) != len("5.0.3-1-g")+7 {
			t.Errorf("after a tag: %q", got)
		}

		// brew's describe cache wins for a user, even over a dirty tree; a
		// developer's dirty tree is described again.
		if err := os.WriteFile(filepath.Join(prefix, "Library", "Homebrew", "brew.sh"), []byte("changed"), 0o644); err != nil {
			t.Fatal(err)
		}
		head := strings.TrimSpace(gitOut(t, prefix, "rev-parse", "HEAD"))
		cache := filepath.Join(prefix, ".git", "describe-cache")
		if err := os.MkdirAll(cache, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cache, head), []byte("5.0.4-cached\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOMEBREW_DEVELOPER", "")
		if got := homebrewVersion(prefix); got != "5.0.4-cached" {
			t.Errorf("with a cache: %q", got)
		}
		t.Setenv("HOMEBREW_DEVELOPER", "1")
		if got := homebrewVersion(prefix); !strings.HasSuffix(got, "-dirty") {
			t.Errorf("a developer's dirty tree: %q", got)
		}
		if err := os.WriteFile(filepath.Join(cache, head), []byte("5.0.4-1-gabcdef0-dirty\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("HOMEBREW_DEVELOPER", "")
		if got := homebrewVersion(prefix); got == "5.0.4-1-gabcdef0-dirty" {
			t.Error("a cached dirty version is not used")
		}
	})
	t.Run("from installed kegs", func(t *testing.T) {
		prefix := t.TempDir()
		for keg, v := range map[string]string{"a/1.0": "4.6.1", "b/2.0": "5.0.2-3-gabc1234", "c/1.0": "5.0.2"} {
			dir := filepath.Join(prefix, "Cellar", keg)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "INSTALL_RECEIPT.json"), []byte(`{"homebrew_version":"`+v+`"}`), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		if got := homebrewVersion(prefix); got != "5.0.2-3-gabc1234" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("nothing to go by", func(t *testing.T) {
		if got := homebrewVersion(t.TempDir()); got != ">=4.3.0 (shallow or no git repository)" {
			t.Errorf("got %q", got)
		}
	})
}
