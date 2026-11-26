package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/install"
	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/sandbox"
)

// Clears the variables brw reads so CI or a developer's shell cannot change expectations.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == sandbox.DisclaimCommand {
		os.Exit(sandbox.DisclaimMain(os.Args[2:], os.Stderr))
	}
	if len(os.Args) > 1 && os.Args[1] == poststep.ChildCommand {
		os.Exit(poststep.ChildMain(os.Args[2:], os.Stdout, os.Stderr, install.ChildActions()))
	}
	for _, k := range []string{"HOMEBREW_NO_INSTALL_CLEANUP", "HOMEBREW_NO_AUTOREMOVE", "HOMEBREW_NO_CLEANUP_FORMULAE", "HOMEBREW_CACHE"} {
		_ = os.Unsetenv(k)
	}
	_ = os.Unsetenv("HOMEBREW_NO_SUDO")
	_ = os.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	os.Exit(m.Run())
}

// shortTempDir is a temporary directory whose path leaves room for a unix socket, which t.TempDir's does not.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "bt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func TestFormatVersions(t *testing.T) {
	kegs := []cellar.Keg{
		{Name: "jq", Version: "1.8.2"},
		{Name: "multi", Version: "1.0"},
		{Name: "multi", Version: "2.0"},
		{Name: "node", Version: "26.11.1"},
	}
	want := "jq 1.8.2\nmulti 1.0 2.0\nnode 26.11.1\n"
	if got := formatVersions(kegs); got != want {
		t.Errorf("formatVersions =\n%q\nwant\n%q", got, want)
	}
	if got := formatVersions(nil); got != "" {
		t.Errorf("formatVersions(nil) = %q", got)
	}
}

func TestWithRootWithoutJournal(t *testing.T) {
	dir := t.TempDir()
	err := withRoot(dir, "", func(r *fsx.Root) error {
		return r.WriteFile("a/b", []byte("x"), 0o644)
	})
	if err != nil {
		t.Fatalf("write without a journal failed: %v", err)
	}
}

func TestWithRootJournalFile(t *testing.T) {
	dir, journal := t.TempDir(), filepath.Join(t.TempDir(), "j.log")
	err := withRoot(dir, journal, func(r *fsx.Root) error {
		return r.WriteFile("a/b", []byte("x"), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(journal); string(got) != "write a/b\n" {
		t.Errorf("journal = %q", got)
	}
}

func TestRunHelp(t *testing.T) {
	commands := []string{"list", "search", "info", "install", "reinstall", "upgrade", "uninstall", "autoremove",
		"leaves", "link", "unlink", "cleanup", "pin", "unpin", "services", "version", "update", "deps", "uses", "outdated", "tag"}
	for _, cmd := range []string{"help", "--help", "-h"} {
		var out bytes.Buffer
		if err := run(&env{out: &out}, cmd, nil); err != nil {
			t.Fatalf("run(%s): %v", cmd, err)
		}
		for _, c := range commands {
			if !strings.Contains(out.String(), "brw "+c) {
				t.Errorf("run(%s) help does not mention %q", cmd, c)
			}
		}
	}
}

func TestRunDispatch(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{{"nope"}, {"dev"}, {"keg-create"}} {
		cmd, rest := args[0], args[1:]
		if err := run(&env{out: &out}, cmd, rest); err == nil {
			t.Errorf("run(%v) should fail", args)
		}
	}
}
