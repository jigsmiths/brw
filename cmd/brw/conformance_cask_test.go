package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// This test installs the same casks with brew and with brw into two fresh
// copies of a brew checkout, each with its own applications folder, and
// compares them; then each forgets and adopts its own, then uninstalls them,
// and they are compared after each.
// It needs a checkout at
// $BRW_ORACLE (default ~/.brw-oracle/hb) and BRW_CONFORMANCE_CASK_INSTALL=1.

// Small casks that nobody has running, since brew's uninstall quits them: an
// app from a zip, one from a disk image, binaries named by their path in the
// prefix, from a zip, and a binary it generates completions from.
var caskInstallSet = []string{"itsycal", "monitorcontrol", "android-platform-tools", "1password-cli"}

func TestConformanceCaskInstall(t *testing.T) {
	if os.Getenv("BRW_CONFORMANCE_CASK_INSTALL") == "" {
		t.Skip("set BRW_CONFORMANCE_CASK_INSTALL=1 to compare cask installs with brew")
	}
	home, _ := os.UserHomeDir()
	oracle := cmpOr(os.Getenv("BRW_ORACLE"), filepath.Join(home, ".brw-oracle", "hb"))
	cache := filepath.Join(filepath.Dir(oracle), "cache")
	bin := filepath.Join(t.TempDir(), "brw")
	sh(t, "", nil, "go", "build", "-o", bin, ".")

	work := workDir(t, filepath.Dir(oracle))
	theirs, mine := filepath.Join(work, "brew"), filepath.Join(work, "brw")
	theirApps, myApps := filepath.Join(work, "apps-brew"), filepath.Join(work, "apps-brw")
	for _, p := range []string{theirs, mine} {
		sh(t, "", nil, "git", "clone", "-q", oracle, p)
	}
	env := []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_CACHE=" + cache}
	brwEnv := append(slices.Clone(env), "HOME="+filepath.Join(work, "home"))
	brewIn := func(prefix string, args ...string) string {
		return sh(t, "", env, filepath.Join(prefix, "bin", "brew"), args...)
	}
	// brew installs from its cached listing; refresh it so both use today's
	// casks. Both prefixes get the links brew update makes.
	brewIn(theirs, "update", "--quiet")
	brewIn(mine, "update", "--quiet")

	brewIn(theirs, append([]string{"install", "--cask", "--appdir", theirApps}, caskInstallSet...)...)
	sh(t, "", brwEnv, bin, append([]string{"install", "--cask", "-prefix", mine, "--appdir", myApps}, caskInstallSet...)...)

	stamp := regexp.MustCompile(`(?m)/\d{14}\.\d{3}(/|$)`)
	normal := func(s, prefix, apps string) string {
		s = strings.ReplaceAll(s, prefix, "<prefix>")
		s = strings.ReplaceAll(s, apps, "<apps>")
		return stamp.ReplaceAllString(s, "/<time>$1")
	}
	compareTrees := func(step string) {
		t.Helper()
		a, b := normal(snapshot(t, theirs), theirs, theirApps), normal(snapshot(t, mine), mine, myApps)
		if d := lineDiff(a, b); len(d) > 0 {
			t.Errorf("%s: prefixes differ:\n%s", step, strings.Join(d, "\n"))
		}
		a, b = snapshot(t, theirApps), snapshot(t, myApps)
		if d := lineDiff(a, b); len(d) > 0 {
			t.Errorf("%s: applications folders differ:\n%s", step, strings.Join(d, "\n"))
		}
	}
	compareTrees("install")
	compareCompletions(t, "install", theirs, mine)
	for _, token := range caskInstallSet {
		meta := filepath.Join("Caskroom", token, ".metadata")
		a := caskMetadata(t, filepath.Join(theirs, meta), theirs, theirApps, home)
		b := caskMetadata(t, filepath.Join(mine, meta), mine, myApps, filepath.Join(work, "home"), home)
		for name := range a {
			if !reflect.DeepEqual(a[name], b[name]) {
				t.Errorf("%s %s:\nbrew: %v\nbrw:  %v", token, name, a[name], b[name])
			}
		}
	}

	// brew accepts what brw installed.
	if got, want := brewIn(mine, "list", "--cask"), sh(t, "", brwEnv, bin, "list", "--cask", "-prefix", mine); got != want {
		t.Errorf("brew list --cask on brw's prefix:\n%s\nbrw:\n%s", got, want)
	}
	for _, token := range caskInstallSet {
		brewIn(mine, "info", "--cask", token)
	}

	// Each forgets its casks, then adopts the apps and links left behind.
	for _, p := range []string{theirs, mine} {
		for _, token := range caskInstallSet {
			if err := os.RemoveAll(filepath.Join(p, "Caskroom", token)); err != nil {
				t.Fatal(err)
			}
		}
	}
	steps := regexp.MustCompile(`(?m)^(==> (Installing|Adopting|Moving|Linking|Binary)|Warning: It seems|Error).*$`)
	adopt := append([]string{"install", "--cask", "--adopt"}, caskInstallSet...)
	a := steps.FindAllString(normal(brewIn(theirs, append(adopt, "--appdir", theirApps)...), theirs, theirApps), -1)
	b := steps.FindAllString(normal(sh(t, "", brwEnv, bin, append(adopt, "-prefix", mine, "--appdir", myApps)...), mine, myApps), -1)
	if d := lineDiff(strings.Join(a, "\n"), strings.Join(b, "\n")); len(d) > 0 {
		t.Errorf("adopt output differs:\n%s", strings.Join(d, "\n"))
	}
	compareTrees("adopt")

	// brw uninstalls as brew does.
	uninstall := append([]string{"uninstall", "--cask"}, caskInstallSet...)
	ua := normal(brewIn(theirs, uninstall...), theirs, theirApps)
	ub := normal(sh(t, "", brwEnv, bin, append([]string{"uninstall", "--cask", "-prefix", mine}, caskInstallSet...)...), mine, myApps)
	if d := lineDiff(ua, ub); len(d) > 0 {
		t.Errorf("uninstall output differs:\n%s", strings.Join(d, "\n"))
	}
	compareTrees("uninstall")
}

// caskMetadata reads a cask's receipt and config, leaving out what differs
// from one install to the next and what brw cannot know.
// homes are replaced in order, so a home inside another comes first.
func caskMetadata(t *testing.T, dir, prefix, apps string, homes ...string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, name := range []string{"INSTALL_RECEIPT.json", "config.json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		// brw runs with its own HOME, which the default folders follow.
		s := strings.NewReplacer(prefix, "<prefix>", apps, "<apps>").Replace(string(b))
		for _, home := range homes {
			s = strings.ReplaceAll(s, home+"/", "<home>/")
		}
		b = []byte(s)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		for _, k := range []string{"homebrew_version", "time", "built_on"} {
			delete(m, k)
		}
		if src, ok := m["source"].(map[string]any); ok {
			delete(src, "tap_git_head") // from the listing each one read
		}
		out[name] = m
	}
	return out
}
