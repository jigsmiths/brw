package main

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This test installs the same formulae with brew and with brw into two fresh
// copies of a brew checkout and compares the results. It needs a checkout at
// $BRW_ORACLE (default ~/.brw-oracle/hb) and BRW_CONFORMANCE_INSTALL=1. Brew
// refuses prefixes under /private/tmp, so the copies live next to the checkout.

// Formulae with :any bottles, so brew can pour them into a long prefix.
var installSet = []string{"jq", "minio", "cayley", "janet", "daemontools", "sbtenv", "docbook", "rustup"}

func TestConformanceInstall(t *testing.T) {
	if os.Getenv("BRW_CONFORMANCE_INSTALL") == "" {
		t.Skip("set BRW_CONFORMANCE_INSTALL=1 to compare installs with brew")
	}
	home, _ := os.UserHomeDir()
	oracle := cmpOr(os.Getenv("BRW_ORACLE"), filepath.Join(home, ".brw-oracle", "hb"))
	cache := filepath.Join(filepath.Dir(oracle), "cache")
	bin := filepath.Join(t.TempDir(), "brw")
	sh(t, "", nil, "go", "build", "-o", bin, ".")

	work, err := os.MkdirTemp(filepath.Dir(oracle), "run-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(work) })
	theirs, mine := filepath.Join(work, "brew"), filepath.Join(work, "brw")
	for _, p := range []string{theirs, mine} {
		sh(t, "", nil, "git", "clone", "-q", oracle, p)
	}
	env := []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_CACHE=" + cache}
	brewIn := func(prefix string, args ...string) string {
		return sh(t, "", env, filepath.Join(prefix, "bin", "brew"), args...)
	}

	brewIn(theirs, append([]string{"install"}, installSet...)...)
	sh(t, "", env, bin, append([]string{"install", "-prefix", mine}, installSet...)...)

	a, b := snapshot(t, theirs), snapshot(t, mine)
	a, b = strings.ReplaceAll(a, theirs, "<prefix>"), strings.ReplaceAll(b, mine, "<prefix>")
	if d := lineDiff(a, b); len(d) > 0 {
		t.Errorf("trees differ:\n%s", strings.Join(d, "\n"))
	}
	for _, name := range installSet {
		ra, rb := receipt(t, theirs, name), receipt(t, mine, name)
		for _, d := range lineDiff(strings.Join(ra, "\n"), strings.Join(rb, "\n")) {
			t.Errorf("%s receipt: %.300s", name, d)
		}
	}

	// brew accepts what brw installed.
	want := sh(t, "", env, bin, "list", "-prefix", mine)
	if got := brewIn(mine, "list", "--formula"); got != want {
		t.Errorf("brew list differs from brw list on brw's prefix\nbrew:\n%s\nbrw:\n%s", got, want)
	}
	for _, name := range installSet {
		brewIn(mine, "info", "--json=v1", name)
	}
	brewIn(mine, "unlink", "jq")
	brewIn(mine, "link", "jq")
	if out := sh(t, "", env, bin, "list", "-prefix", theirs); out != brewIn(theirs, "list", "--formula") {
		t.Errorf("brw list differs from brew list on brew's prefix:\n%s", out)
	}
	uninstall := append([]string{"uninstall", "--force", "--ignore-dependencies"}, installSet...)
	brewIn(theirs, uninstall...)
	brewIn(mine, uninstall...)
	a, b = snapshot(t, theirs), snapshot(t, mine)
	a, b = strings.ReplaceAll(a, theirs, "<prefix>"), strings.ReplaceAll(b, mine, "<prefix>")
	if d := lineDiff(a, b); len(d) > 0 {
		t.Errorf("trees differ after brew uninstalls the same formulae:\n%s", strings.Join(d, "\n"))
	}
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func sh(t *testing.T, dir string, env []string, name string, args ...string) string {
	t.Helper()
	cmd := osexec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out)
}

// snapshot lists the prefix like `find`, leaving out the checkout, the Cellar
// and brew's locks and scratch space.
func snapshot(t *testing.T, prefix string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(prefix, p)
		switch rel {
		case ".", ".git", "Library", "Cellar", "var/homebrew/locks", "var/homebrew/tmp":
			if rel == "." {
				return nil
			}
			return fs.SkipDir
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			target, _ := os.Readlink(p)
			lines = append(lines, "L "+rel+" -> "+target)
		case d.IsDir():
			lines = append(lines, "D "+rel)
		default:
			lines = append(lines, "F "+rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

// lineDiff returns the lines in only one of a and b, ignoring brew's own var/log.
func lineDiff(a, b string) []string {
	as, bs := strings.Split(a, "\n"), strings.Split(b, "\n")
	var out []string
	for _, l := range as {
		if !slices.Contains(bs, l) && l != "D var/log" {
			out = append(out, "brew only: "+l)
		}
	}
	for _, l := range bs {
		if !slices.Contains(as, l) {
			out = append(out, "brw only:  "+l)
		}
	}
	return out
}

// receipt returns the receipt's fields without the install time, sorted.
func receipt(t *testing.T, prefix, name string) []string {
	t.Helper()
	matches, _ := filepath.Glob(filepath.Join(prefix, "Cellar", name, "*", "INSTALL_RECEIPT.json"))
	if len(matches) != 1 {
		t.Fatalf("%s: %d receipts", name, len(matches))
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "time")
	var out []string
	for k, v := range m {
		var c bytes.Buffer
		_ = json.Compact(&c, v)
		out = append(out, k+"="+strings.ReplaceAll(c.String(), prefix, "<prefix>"))
	}
	slices.Sort(out)
	return out
}
