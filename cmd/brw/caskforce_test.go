package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstallForceReplacesAnExistingFont(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	t.Setenv("HOMEBREW_CASK_OPTS", "")
	s := newStore(t)
	s.fontCask("myfont")
	e, prefix := s.env()
	fonts := filepath.Join(t.TempDir(), "Fonts")
	target := filepath.Join(fonts, "Font.ttf")
	if err := os.MkdirAll(fonts, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--force", "--fontdir="+fonts, "--cask", "myfont"); err != nil {
		t.Fatalf("forced install: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(target); string(b) != "font" {
		t.Errorf("font = %q, want it replaced", b)
	}
}

func TestInstallRejectsForceWithAdopt(t *testing.T) {
	s := newStore(t)
	e, prefix := s.env()
	_, err := runCmd(t, e, "install", "-prefix", prefix, "--force", "--adopt", "--cask", "something")
	if want := "Options --adopt and --force are mutually exclusive."; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}
