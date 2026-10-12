package main

import (
	"os"
	osexec "os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// This test installs formulae with brew into two fresh copies of a brew
// checkout, then removes them with brew in one and brw in the other and
// compares the prefixes after each step. It needs the checkout described in
// conformance_install_test.go and BRW_CONFORMANCE_UNINSTALL=1.

// oniguruma comes in as the dependency of jq.
var uninstallSet = []string{"jq", "minio", "cayley", "sbtenv", "rustup"}

func TestConformanceUninstall(t *testing.T) {
	if os.Getenv("BRW_CONFORMANCE_UNINSTALL") == "" {
		t.Skip("set BRW_CONFORMANCE_UNINSTALL=1 to compare uninstalls with brew")
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
	// brw does not remove unneeded dependencies yet, so brew is told not to either.
	env := []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_NO_AUTOREMOVE=1", "HOMEBREW_CACHE=" + cache}
	brwEnv := append(slices.Clone(env), "HOME="+filepath.Join(work, "home"))

	for _, p := range []string{theirs, mine} {
		sh(t, "", env, filepath.Join(p, "bin", "brew"), append([]string{"install"}, uninstallSet...)...)
	}

	failed := false
	step := func(name string, wantFail bool, args ...string) {
		t.Helper()
		_, errBrew := try(env, filepath.Join(theirs, "bin", "brew"), append([]string{"uninstall"}, args...)...)
		_, errBrw := try(brwEnv, bin, append([]string{"uninstall", "-prefix", mine}, args...)...)
		if (errBrew != nil) != wantFail || (errBrw != nil) != wantFail {
			t.Errorf("%s: brew failed = %v, brw failed = %v, want %v", name, errBrew != nil, errBrw != nil, wantFail)
			failed = true
		}
		compare(t, name, theirs, mine)
	}

	step("refusing to remove a dependency", true, "oniguruma")

	// A second version of jq, and a pin on minio, made the same way in both.
	for _, p := range []string{theirs, mine} {
		jq := filepath.Join(p, "Cellar", "jq")
		versions, _ := os.ReadDir(jq)
		if len(versions) != 1 {
			t.Fatalf("jq versions in %s: %v", p, versions)
		}
		sh(t, "", nil, "cp", "-R", filepath.Join(jq, versions[0].Name()), filepath.Join(jq, "0.9"))
		minio, _ := os.ReadDir(filepath.Join(p, "Cellar", "minio"))
		if err := os.MkdirAll(filepath.Join(p, "var/homebrew/pinned"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../../../Cellar/minio/"+minio[0].Name(), filepath.Join(p, "var/homebrew/pinned/minio")); err != nil {
			t.Fatal(err)
		}
	}
	compare(t, "after adding a version and a pin", theirs, mine)

	step("the linked version of two", false, "jq")
	step("a pinned formula", false, "minio")
	step("all versions", false, "--force", "jq")
	step("several names at once", false, "cayley", "sbtenv")
	step("forcing past a dependent", false, "--ignore-dependencies", "--force", "oniguruma", "minio", "rustup", "xz")

	// With everything removed, no link, opt record or linked record may be left
	// that brew does not leave as well (post_install can make some).
	if !failed {
		brewLeft := snapshot(t, theirs)
		for _, line := range strings.Split(snapshot(t, mine), "\n") {
			if !strings.Contains(line, "Cellar/") && !strings.HasPrefix(line, "L opt/") && !strings.HasPrefix(line, "L var/homebrew/linked/") {
				continue
			}
			if !strings.Contains(brewLeft, strings.ReplaceAll(line, mine, theirs)) {
				t.Errorf("left behind after removing everything: %s", line)
			}
		}
	}
}

// try runs a command and returns its output and error without failing the test.
func try(env []string, name string, args ...string) (string, error) {
	cmd := osexec.Command(name, args...)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// compare reports every difference between the two prefixes, including which
// kegs are left in the Cellar.
func compare(t *testing.T, step, theirs, mine string) {
	t.Helper()
	a, b := snapshot(t, theirs), snapshot(t, mine)
	a, b = strings.ReplaceAll(a, theirs, "<prefix>"), strings.ReplaceAll(b, mine, "<prefix>")
	if d := lineDiff(a, b); len(d) > 0 {
		t.Errorf("%s: trees differ:\n%s", step, strings.Join(d, "\n"))
	}
	if c1, c2 := cellarList(t, theirs), cellarList(t, mine); !slices.Equal(c1, c2) {
		t.Errorf("%s: Cellar differs\nbrew: %v\nbrw:  %v", step, c1, c2)
	}
}

// cellarList is every rack and keg in the Cellar, links included.
func cellarList(t *testing.T, prefix string) []string {
	t.Helper()
	var out []string
	racks, _ := os.ReadDir(filepath.Join(prefix, "Cellar"))
	for _, r := range racks {
		out = append(out, r.Name())
		kegs, _ := os.ReadDir(filepath.Join(prefix, "Cellar", r.Name()))
		for _, k := range kegs {
			out = append(out, r.Name()+"/"+k.Name())
		}
	}
	return out
}
