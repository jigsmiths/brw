package main

import (
	"bytes"
	"os"
	osexec "os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/platform"
)

// These tests compare brw with the real brew on this machine's prefix.
// Run them with BRW_CONFORMANCE=1.

func conformanceEnv(t *testing.T) *env {
	t.Helper()
	if os.Getenv("BRW_CONFORMANCE") == "" {
		t.Skip("set BRW_CONFORMANCE=1 to compare with brew")
	}
	return &env{err: &bytes.Buffer{}, host: platform.Current(macVersion())}
}

// brew returns brew's stdout, or false if it failed.
func brew(t *testing.T, args ...string) (string, bool) {
	t.Helper()
	cmd := osexec.Command("brew", args...)
	cmd.Env = append(os.Environ(), "HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1")
	out, err := cmd.Output()
	return string(out), err == nil
}

func same(t *testing.T, label, got, want string) bool {
	t.Helper()
	if got == want {
		return true
	}
	t.Errorf("%s differs\nbrw:\n%s\nbrew:\n%s", label, got, want)
	return false
}

func TestConformanceList(t *testing.T) {
	e := conformanceEnv(t)
	want, _ := brew(t, "list", "--formula")
	got, err := runCmd(t, e, "list")
	if err != nil {
		t.Fatal(err)
	}
	same(t, "list", got, want)
	want, _ = brew(t, "list", "--formula", "--versions")
	got, err = runCmd(t, e, "list", "--versions")
	if err != nil {
		t.Fatal(err)
	}
	same(t, "list --versions", got, want)
}

func installedNames(t *testing.T, e *env) []string {
	t.Helper()
	got, err := runCmd(t, e, "list", "--versions")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for line := range strings.SplitSeq(strings.TrimSpace(got), "\n") {
		names = append(names, strings.Fields(line)[0])
	}
	return names
}

func TestConformanceDeps(t *testing.T) {
	e := conformanceEnv(t)
	for _, name := range installedNames(t, e) {
		for _, flags := range [][]string{nil, {"--1"}, {"--tree"}} {
			args := append(slices.Clone(flags), name)
			got, err := runCmd(t, e, "deps", args...)
			if err != nil {
				t.Logf("skipped brw deps %v: %v", args, err)
				continue
			}
			want, ok := brew(t, append([]string{"deps"}, args...)...)
			if ok {
				same(t, "deps "+strings.Join(args, " "), got, want)
			}
		}
	}
}

func TestConformanceUses(t *testing.T) {
	e := conformanceEnv(t)
	for _, name := range installedNames(t, e) {
		for _, flags := range [][]string{{"--installed"}, {"--installed", "--recursive"}} {
			args := append(slices.Clone(flags), name)
			got, err := runCmd(t, e, "uses", args...)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := brew(t, append([]string{"uses", "--formula"}, args...)...)
			same(t, "uses "+strings.Join(args, " "), got, want)
		}
	}
}

func TestConformanceOutdated(t *testing.T) {
	e := conformanceEnv(t)
	got, err := runCmd(t, e, "outdated")
	if err != nil {
		t.Fatal(err)
	}
	want, _ := brew(t, "outdated", "--formula", "--quiet")
	same(t, "outdated", got, want)
}

// Brew adds spelling suggestions to text searches, so brw's answer may be a subset.
func TestConformanceSearch(t *testing.T) {
	e := conformanceEnv(t)
	for _, q := range []string{"jq", "json", "sqlite", "python 3", "c++", "node", "/^lib/", "/json$/", "/^JSON/"} {
		got, err := runCmd(t, e, "search", q)
		if err != nil {
			t.Logf("search %q: %v", q, err)
		}
		wantOut, _ := brew(t, "search", "--formula", q)
		want := strings.Fields(wantOut)
		for _, name := range strings.Fields(got) {
			if !slices.Contains(want, name) {
				t.Errorf("search %q: brw found %s, brew did not", q, name)
			}
		}
		if strings.HasPrefix(q, "/") {
			same(t, "search "+q, got, wantOut)
		}
	}
}
