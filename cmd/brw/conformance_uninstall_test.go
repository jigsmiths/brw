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
// checkout, links, unlinks and removes them with brew in one and brw in the other and
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

	work := workDir(t, filepath.Dir(oracle))
	theirs, mine := filepath.Join(work, "brew"), filepath.Join(work, "brw")
	for _, p := range []string{theirs, mine} {
		sh(t, "", nil, "git", "clone", "-q", oracle, p)
	}
	env := []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_CACHE=" + cache}
	brwEnv := append(slices.Clone(env), "HOME="+filepath.Join(work, "home"))

	for _, p := range []string{theirs, mine} {
		sh(t, "", env, filepath.Join(p, "bin", "brew"), append([]string{"install"}, uninstallSet...)...)
	}

	failed := false
	// step runs the same command with both tools and compares the prefixes. It
	// returns what each printed, with the prefix made the same.
	step := func(name string, wantFail bool, extraEnv []string, cmd string, args ...string) (outBrew, outBrw string) {
		t.Helper()
		outBrew, errBrew := try(append(slices.Clone(env), extraEnv...), filepath.Join(theirs, "bin", "brew"), append([]string{cmd}, args...)...)
		outBrw, errBrw := try(append(slices.Clone(brwEnv), extraEnv...), bin, append([]string{cmd, "-prefix", mine}, args...)...)
		if (errBrew != nil) != wantFail || (errBrw != nil) != wantFail {
			t.Errorf("%s: brew failed = %v, brw failed = %v, want %v\nbrew: %s\nbrw: %s", name, errBrew != nil, errBrw != nil, wantFail, outBrew, outBrw)
			failed = true
		}
		compare(t, name, theirs, mine)
		return strings.ReplaceAll(outBrew, theirs, "<prefix>"), strings.ReplaceAll(outBrw, mine, "<prefix>")
	}
	// sameLine checks that both tools printed the line that starts with prefix.
	sameLine := func(name, outBrew, outBrw, start string) {
		t.Helper()
		find := func(out string) string {
			for _, l := range strings.Split(out, "\n") {
				if strings.HasPrefix(l, start) {
					return l
				}
			}
			return ""
		}
		if a, b := find(outBrew), find(outBrw); a == "" || a != b {
			t.Errorf("%s: the %q line differs\nbrew: %q\nbrw:  %q", name, start, a, b)
		}
	}
	noAutoremove := []string{"HOMEBREW_NO_AUTOREMOVE=1"}

	// Linking and unlinking, and what they print.
	a, b := step("unlink", false, nil, "unlink", "jq")
	sameLine("unlink", a, b, "Unlinking ")
	a, b = step("link again", false, nil, "link", "jq")
	sameLine("link", a, b, "Linking ")
	step("link when already linked", false, nil, "link", "jq")
	step("a keg-only formula without --force", false, nil, "link", "rustup")
	a, b = step("a keg-only formula with --force", false, nil, "link", "--force", "rustup")
	sameLine("link --force", a, b, "Linking ")
	a, b = step("unlink a keg-only formula", false, nil, "unlink", "rustup")
	sameLine("unlink keg-only", a, b, "Unlinking ")
	// brew's own dry run of link creates empty directories, so only brw's is
	// run: it has to change nothing.
	beforeDry := snapshot(t, mine)
	for _, args := range [][]string{{"link", "-n", "--force", "rustup"}, {"unlink", "-n", "jq"}} {
		out, err := try(brwEnv, bin, append([]string{args[0], "-prefix", mine}, args[1:]...)...)
		if err != nil || !strings.Contains(out, "would ") {
			t.Errorf("brw %v: %q, %v", args, out, err)
		}
	}
	if snapshot(t, mine) != beforeDry {
		t.Error("a dry run of brw link or unlink changed the prefix")
	}

	step("refusing to remove a dependency", true, nil, "uninstall", "oniguruma")

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

	step("the linked version of two", false, nil, "uninstall", "jq")
	step("a pinned formula", false, nil, "uninstall", "minio")

	// With autoremove off, removing jq leaves oniguruma behind unneeded.
	step("all versions, autoremove off", false, noAutoremove, "uninstall", "--force", "jq")
	list := func(tool string) string {
		if tool == "brew" {
			out, _ := try(env, filepath.Join(theirs, "bin", "brew"), "autoremove", "--dry-run")
			return out
		}
		out, _ := try(brwEnv, bin, "autoremove", "-prefix", mine, "--dry-run")
		return out
	}
	if a, b := list("brew"), list("brw"); a != b || !strings.Contains(a, "oniguruma") {
		t.Errorf("autoremove --dry-run differs\nbrew: %q\nbrw:  %q", a, b)
	}
	compare(t, "after the dry run", theirs, mine)
	step("autoremove", false, nil, "autoremove")

	step("several names at once", false, nil, "uninstall", "cayley", "sbtenv")
	step("a dependency of an installed formula", true, nil, "uninstall", "xz")
	for _, p := range []string{theirs, mine} {
		if err := os.Remove(filepath.Join(p, "var/homebrew/pinned/minio")); err != nil {
			t.Fatal(err)
		}
	}
	step("the rest, and the dependency that goes with it", false, nil, "uninstall", "minio", "rustup")

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
