package main

import (
	"os"
	"path/filepath"
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
