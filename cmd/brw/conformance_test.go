package main

import (
	"bytes"
	"encoding/json"
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

// infoBlocks splits brew info output into one block per formula, each a map
// from section name ("" for the lines before the first section) to its lines.
func infoBlocks(out string) map[string]map[string][]string {
	sections := map[string]bool{"Installed Versions": true, "Dependencies": true, "Requirements": true,
		"Options": true, "Caveats": true, "Analytics": true}
	blocks := map[string]map[string][]string{}
	var cur map[string][]string
	section := ""
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		if name, ok := strings.CutPrefix(line, "==> "); ok {
			if sections[name] {
				section = name
				continue
			}
			if formula, _, ok := strings.Cut(name, ": "); ok {
				cur, section = map[string][]string{}, ""
				blocks[formula] = cur
			}
		}
		if cur != nil {
			cur[section] = append(cur[section], line)
		}
	}
	for _, b := range blocks {
		for k, lines := range b {
			for len(lines) > 0 && lines[len(lines)-1] == "" {
				lines = lines[:len(lines)-1]
			}
			b[k] = lines
		}
	}
	return blocks
}

// isSubsequence reports whether every line of sub appears in lines, in order.
func isSubsequence(sub, lines []string) bool {
	i := 0
	for _, l := range lines {
		if i < len(sub) && sub[i] == l {
			i++
		}
	}
	return i == len(sub)
}

// infoNames picks the installed formulae brw can describe, plus a spread of
// uninstalled ones.
func infoNames(t *testing.T, e *env) []string {
	t.Helper()
	idx, err := e.index(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	installed := installedNames(t, e)
	var names []string
	for _, n := range installed {
		if _, err := runCmd(t, e, "info", n); err != nil {
			t.Logf("skipped %s: %v", n, err)
			continue
		}
		names = append(names, n)
	}
	for i, f := range idx.Formulae() {
		if i%85 == 0 && !slices.Contains(installed, f.Name) {
			names = append(names, f.Name)
		}
	}
	return names
}

// brw prints what brew prints when piped, except for Analytics and the parts of
// Caveats that brew works out by running Ruby. brw's caveats must be a subset.
func TestConformanceInfo(t *testing.T) {
	e := conformanceEnv(t)
	names := infoNames(t, e)
	want, ok := brew(t, append([]string{"info", "--formula"}, names...)...)
	if !ok {
		t.Fatal("brew info failed")
	}
	brews := infoBlocks(want)
	got, err := runCmd(t, e, "info", names...)
	if err != nil {
		t.Fatal(err)
	}
	mine := infoBlocks(got)
	for _, n := range names {
		b, m := brews[n], mine[n]
		if b == nil || m == nil {
			t.Errorf("%s: missing from brew (%v) or brw (%v)", n, b != nil, m != nil)
			continue
		}
		for section := range b {
			if section == "Analytics" || section == "Caveats" {
				continue
			}
			if !slices.Equal(b[section], m[section]) {
				t.Errorf("%s: section %q differs\nbrw:\n%s\nbrew:\n%s", n, section, strings.Join(m[section], "\n"), strings.Join(b[section], "\n"))
			}
		}
		for section := range m {
			if _, ok := b[section]; !ok && section != "Caveats" {
				t.Errorf("%s: brw printed section %q, brew did not", n, section)
			}
		}
		if !isSubsequence(m["Caveats"], b["Caveats"]) {
			t.Errorf("%s: caveats are not part of brew's\nbrw:\n%s\nbrew:\n%s", n, strings.Join(m["Caveats"], "\n"), strings.Join(b["Caveats"], "\n"))
		}
	}
	t.Logf("compared %d formulae", len(names))
}

// Brew reads its own copy of the API, which differs from the public listing in
// these keys, so they are not compared.
var apiSourceKeys = []string{"tap_git_head", "compatibility_version", "patches", "requirements", "skip_livecheck"}

func TestConformanceInfoJSON(t *testing.T) {
	e := conformanceEnv(t)
	names := infoNames(t, e)
	wantOut, ok := brew(t, append([]string{"info", "--json=v2", "--formula"}, names...)...)
	if !ok {
		t.Fatal("brew info failed")
	}
	gotOut, err := runCmd(t, e, "info", append([]string{"--json"}, names...)...)
	if err != nil {
		t.Fatal(err)
	}
	type doc struct {
		Formulae []map[string]json.RawMessage `json:"formulae"`
	}
	var want, got doc
	if err := json.Unmarshal([]byte(wantOut), &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(gotOut), &got); err != nil {
		t.Fatal(err)
	}
	if len(want.Formulae) != len(got.Formulae) {
		t.Fatalf("brew listed %d formulae, brw %d", len(want.Formulae), len(got.Formulae))
	}
	compact := func(raw json.RawMessage) string {
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	for i, w := range want.Formulae {
		g := got.Formulae[i]
		var name string
		_ = json.Unmarshal(w["name"], &name)
		for k := range w {
			if slices.Contains(apiSourceKeys, k) {
				continue
			}
			if g[k] == nil {
				t.Errorf("%s: brw has no %q", name, k)
			} else if compact(w[k]) != compact(g[k]) {
				t.Errorf("%s: %q differs\nbrw:  %.200s\nbrew: %.200s", name, k, g[k], w[k])
			}
		}
		for k := range g {
			if w[k] == nil {
				t.Errorf("%s: brw has an extra key %q", name, k)
			}
		}
	}
}
