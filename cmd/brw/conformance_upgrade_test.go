package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// This test compares brew update and brew upgrade with brw's. It needs the
// checkout described in conformance_install_test.go and BRW_CONFORMANCE_UPGRADE=1.
//
// Both tools install the same formulae into their own prefix, some are pinned or
// unlinked, and then every keg is renamed to 0.0.1 in both prefixes so that the
// formulae count as outdated. The upgrades must leave the same prefix behind.

// jq needs oniguruma, sqlite is keg-only and needs readline, just is pinned and
// c-ares is unlinked.
var upgradeSet = []string{"jq", "sqlite", "just", "c-ares"}

func TestConformanceUpgrade(t *testing.T) {
	if os.Getenv("BRW_CONFORMANCE_UPGRADE") == "" {
		t.Skip("set BRW_CONFORMANCE_UPGRADE=1 to compare upgrades with brew")
	}
	home, _ := os.UserHomeDir()
	oracle := cmpOr(os.Getenv("BRW_ORACLE"), filepath.Join(home, ".brw-oracle", "hb"))
	cache := filepath.Join(filepath.Dir(oracle), "cache")
	bin := filepath.Join(t.TempDir(), "brw")
	sh(t, "", nil, "go", "build", "-o", bin, ".")

	work := workDir(t, filepath.Dir(oracle))
	theirs, mine := filepath.Join(work, "brew"), filepath.Join(work, "brw")
	for _, p := range []string{theirs, mine} {
		sh(t, "", nil, "git", "clone", "-q", oracle, p)
	}
	// The shared cache may hold a listing older than the one brw fetches. A
	// throwaway copy refreshes it, so both tools see the same versions.
	refresh := filepath.Join(work, "refresh")
	sh(t, "", nil, "git", "clone", "-q", oracle, refresh)
	sh(t, "", []string{"HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_CACHE=" + cache}, filepath.Join(refresh, "bin", "brew"), "update")
	env := []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_CACHE=" + cache}
	brwEnv := append(slices.Clone(env), "HOME="+filepath.Join(work, "home"))
	brewIn := func(prefix string, args ...string) string {
		return sh(t, "", env, filepath.Join(prefix, "bin", "brew"), args...)
	}
	brwIn := func(args ...string) string {
		return sh(t, "", brwEnv, bin, append(args[:1:1], append([]string{"-prefix", mine}, args[1:]...)...)...)
	}
	brwUpdate := func() string { return sh(t, "", brwEnv, bin, "update", "--no-self") }

	brewIn(theirs, append([]string{"install"}, upgradeSet...)...)
	brwIn(append([]string{"install"}, upgradeSet...)...)
	brewIn(theirs, "pin", "just")
	brwIn("pin", "just")
	brewIn(theirs, "unlink", "c-ares")
	brwIn("unlink", "c-ares")
	compare(t, "after install, pin and unlink", theirs, mine)

	names := cellarRacks(t, mine)
	for _, p := range []string{theirs, mine} {
		age(t, p, names)
	}
	compare(t, "after aging", theirs, mine)

	// Both see the same formulae as outdated.
	outdated := func(out string) []string {
		lines := strings.Fields(out)
		slices.Sort(lines)
		return lines
	}
	a := outdated(brewIn(theirs, "outdated", "--formula", "--quiet"))
	b := outdated(brwIn("outdated", "--quiet"))
	if !slices.Equal(a, b) {
		t.Errorf("outdated differs\nbrew: %v\nbrw:  %v", a, b)
	}

	brewIn(theirs, "update")
	brwUpdate()
	brewIn(theirs, "upgrade")
	brwIn("upgrade")

	// brew update links brew's own completions, man pages and docs, which brw
	// update has no reason to do. brew's first run in the brw prefix also fetches
	// its Ruby, so that happens before the doctor output is compared.
	for _, l := range []string{"etc/bash_completion.d/brew", "share/doc/homebrew", "share/fish/vendor_completions.d/brew.fish",
		"share/man/man1/README.md", "share/man/man1/brew.1", "share/zsh/site-functions/_brew"} {
		if err := os.Remove(filepath.Join(theirs, l)); err != nil {
			t.Error(err)
		}
	}
	_, _ = try(env, filepath.Join(mine, "bin", "brew"), "doctor")
	compare(t, "after upgrade", theirs, mine)
	for _, name := range names {
		if name == "just" {
			continue // pinned, so still the old keg in both
		}
		for _, d := range kegDiff(t, theirs, mine, name) {
			t.Errorf("%s keg: %s", name, d)
		}
		ra, rb := receipt(t, theirs, name), receipt(t, mine, name)
		for _, d := range lineDiff(strings.Join(ra, "\n"), strings.Join(rb, "\n")) {
			t.Errorf("%s receipt: %.300s", name, d)
		}
		s := smokeFormula(mine, name)
		for _, p := range s.Problems {
			t.Errorf("%s: %s", name, p)
		}
	}
	if left := outdated(brwIn("outdated", "--quiet")); !slices.Equal(left, []string{"just"}) {
		t.Errorf("outdated after upgrade = %v, want only the pinned just", left)
	}
	doctor := func(prefix string) string {
		out, _ := try(env, filepath.Join(prefix, "bin", "brew"), "doctor")
		return strings.ReplaceAll(out, prefix, "<prefix>")
	}
	for _, d := range lineDiff(doctor(theirs), doctor(mine)) {
		t.Errorf("brew doctor differs: %s", d)
	}
}

// cellarRacks lists the racks in the prefix's Cellar.
func cellarRacks(t *testing.T, prefix string) []string {
	t.Helper()
	racks, err := os.ReadDir(filepath.Join(prefix, "Cellar"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range racks {
		names = append(names, r.Name())
	}
	return names
}

// age renames the kegs of the formulae to 0.0.1 and repoints every symlink
// that went into them.
func age(t *testing.T, prefix string, names []string) {
	t.Helper()
	old := map[string]string{} // name to the version that was installed
	for _, name := range names {
		kegs, err := os.ReadDir(filepath.Join(prefix, "Cellar", name))
		if err != nil || len(kegs) != 1 {
			t.Fatalf("%s: want one keg, got %v (%v)", name, kegs, err)
		}
		old[name] = kegs[0].Name()
		if err := os.Rename(filepath.Join(prefix, "Cellar", name, old[name]), filepath.Join(prefix, "Cellar", name, "0.0.1")); err != nil {
			t.Fatal(err)
		}
	}
	var res []*regexp.Regexp
	var to []string
	for _, name := range names {
		res = append(res, regexp.MustCompile(`/`+regexp.QuoteMeta(name)+`/`+regexp.QuoteMeta(old[name])+`(/|$)`))
		to = append(to, "/"+name+"/0.0.1$1")
	}
	err := filepath.WalkDir(prefix, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && (d.Name() == ".git" || p == filepath.Join(prefix, "Library")) {
			return fs.SkipDir
		}
		if d.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(p)
		if err != nil {
			return err
		}
		fixed := target
		for i, re := range res {
			fixed = re.ReplaceAllString(fixed, to[i])
		}
		if fixed == target {
			return nil
		}
		if err := os.Remove(p); err != nil {
			return err
		}
		return os.Symlink(fixed, p)
	})
	if err != nil {
		t.Fatal(err)
	}
}
