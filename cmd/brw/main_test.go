package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
)

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

func TestRunDispatch(t *testing.T) {
	var out bytes.Buffer
	for _, args := range [][]string{{"nope"}, {"dev"}, {"dev", "nope"}, {"keg-create"}} {
		cmd, rest := args[0], args[1:]
		if err := run(&env{out: &out}, cmd, rest); err == nil {
			t.Errorf("run(%v) should fail", args)
		}
	}
}

func TestDevCommandsMovedOffTopLevel(t *testing.T) {
	for _, cmd := range []string{"keg-create", "keg-remove", "hold-lock"} {
		err := run(&env{out: &bytes.Buffer{}}, cmd, nil)
		if err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%s at top level: %v", cmd, err)
		}
	}
}

func TestDevKegRoundTrip(t *testing.T) {
	prefix := t.TempDir()
	flags := []string{"-prefix", prefix, "-name", "demo", "-version", "1.0"}
	if err := run(&env{out: &bytes.Buffer{}}, "dev", append([]string{"keg-create"}, flags...)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := run(&env{out: &out}, "list", []string{"-versions", "-prefix", prefix}); err != nil || out.String() != "demo 1.0\n" {
		t.Fatalf("list = %q, %v", out.String(), err)
	}
	if err := run(&env{out: &bytes.Buffer{}}, "dev", append([]string{"keg-remove"}, flags...)); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := run(&env{out: &out}, "list", []string{"-versions", "-prefix", prefix}); err != nil || out.String() != "" {
		t.Fatalf("list after remove = %q, %v", out.String(), err)
	}
}
