package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// This test installs real old versions of casks with brew into two fresh
// copies of a brew checkout, then upgrades one with brew and the other with
// brw and compares them. Afterwards neither tool may find anything left to
// upgrade. It needs the checkout described in conformance_cask_test.go and
// BRW_CONFORMANCE_CASK_UPGRADE=1.

// oldCasks are cask files from homebrew-cask's history, older than today's
// listing: an app that updates itself and quits on upgrade, from a zip; a
// binary; and an app with a binary it generates completions from, as zed
// does. claude and zed are left out: their upgrade quits the copies the user
// has open.
var oldCasks = []struct{ token, path, commit, version string }{
	{"itsycal", "Casks/i/itsycal.rb", "24d2b0f0ce07c095284e9d4e4e8bae2c403e8f13", "0.28.0"},
	{"claude-code", "Casks/c/claude-code.rb", "4beccf4bcd0f327fd2675e6751725c42ddb2cf58", "2.1.311"},
	{"otty", "Casks/o/otty.rb", "51fb3a5fe0fae37046830edc2e1635adf8f59f38", "1.6.2"},
}

// completionDirs are where casks' generated completions go.
var completionDirs = []string{"etc/bash_completion.d", "share/zsh/site-functions", "share/fish/vendor_completions.d", "share/pwsh/completions"}

// compareCompletions reports completion files whose contents differ.
func compareCompletions(t *testing.T, step, theirs, mine string) {
	t.Helper()
	for _, dir := range completionDirs {
		entries, _ := os.ReadDir(filepath.Join(theirs, dir))
		for _, e := range entries {
			rel := filepath.Join(dir, e.Name())
			a, errA := os.ReadFile(filepath.Join(theirs, rel))
			b, errB := os.ReadFile(filepath.Join(mine, rel))
			if errA != nil || errB != nil {
				continue // the trees' comparison names a missing file
			}
			if strings.ReplaceAll(string(a), theirs, "<prefix>") != strings.ReplaceAll(string(b), mine, "<prefix>") {
				t.Errorf("%s: %s differs (%d bytes from brew, %d from brw)", step, rel, len(a), len(b))
			}
		}
	}
}

func TestConformanceCaskUpgrade(t *testing.T) {
	if os.Getenv("BRW_CONFORMANCE_CASK_UPGRADE") == "" {
		t.Skip("set BRW_CONFORMANCE_CASK_UPGRADE=1 to compare cask upgrades with brew")
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
	brwIn := func(args ...string) string {
		return sh(t, "", brwEnv, bin, append(args[:1:1], append([]string{"-prefix", mine}, args[1:]...)...)...)
	}
	brewIn(theirs, "update", "--quiet")
	brewIn(mine, "update", "--quiet")

	// brew installs a cask file only from a tap, so the old files go into a
	// homebrew/cask tap of their own, which is removed once they are in. The
	// casks then count as installed from homebrew/cask, as they would be.
	files := map[string][]byte{}
	tokens := make([]string, len(oldCasks))
	for i, c := range oldCasks {
		files[c.path] = fetchOldCask(t, c.commit, c.path)
		tokens[i] = c.token
	}
	for _, p := range []struct{ prefix, apps string }{{theirs, theirApps}, {mine, myApps}} {
		tap := filepath.Join(p.prefix, "Library/Taps/homebrew/homebrew-cask")
		for path, b := range files {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(tap, path)), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(tap, path), b, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		sh(t, tap, nil, "git", "init", "-q")
		sh(t, tap, nil, "git", "add", "-A")
		sh(t, tap, nil, "git", "-c", "user.name=brw", "-c", "user.email=brw@example.com", "commit", "-qm", "old casks")
		sh(t, "", append(slices.Clone(env), "HOMEBREW_NO_INSTALL_FROM_API=1"), filepath.Join(p.prefix, "bin", "brew"),
			append([]string{"install", "--cask", "--appdir", p.apps}, tokens...)...)
		if err := os.RemoveAll(tap); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range oldCasks {
		if _, err := os.Stat(filepath.Join(mine, "Caskroom", c.token, c.version)); err != nil {
			t.Fatalf("%s %s is not installed: %v", c.token, c.version, err)
		}
	}

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
		if d := lineDiff(snapshot(t, theirApps), snapshot(t, myApps)); len(d) > 0 {
			t.Errorf("%s: applications folders differ:\n%s", step, strings.Join(d, "\n"))
		}
	}
	compareTrees("old install")
	compareCompletions(t, "old install", theirs, mine)

	// Both see the same casks as outdated, and every old one is.
	quiet := func(out string) []string {
		lines := strings.Fields(out)
		slices.Sort(lines)
		return lines
	}
	a := quiet(brewIn(theirs, "outdated", "--cask", "--quiet"))
	b := quiet(brwIn("outdated", "--cask", "--quiet"))
	if !slices.Equal(a, b) {
		t.Errorf("outdated differs\nbrew: %v\nbrw:  %v", a, b)
	}

	steps := regexp.MustCompile(`(?m)^(==> |  \S+ -> |\S+ +\S+ +-> |.*successfully upgraded|Warning|Error).*$`)
	upgrade := func(out, prefix, apps string) string {
		var keep []string
		for _, l := range steps.FindAllString(normal(out, prefix, apps), -1) {
			// brew's cleanup is of its own download cache, which brw does not use.
			if l != "==> Cleanup" {
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	// brw does not ask before upgrading, so brew is told not to either.
	ua := upgrade(brewIn(theirs, "upgrade", "--cask", "--no-ask"), theirs, theirApps)
	ub := upgrade(brwIn("upgrade", "--cask"), mine, myApps)
	if d := lineDiff(ua, ub); len(d) > 0 {
		t.Errorf("upgrade output differs:\n%s\nbrew:\n%s\nbrw:\n%s", strings.Join(d, "\n"), ua, ub)
	}
	compareTrees("upgrade")
	compareCompletions(t, "upgrade", theirs, mine)
	if _, err := os.Stat(filepath.Join(mine, "share/zsh/site-functions/_otty")); err != nil {
		t.Errorf("brw did not generate otty's zsh completions: %v", err)
	}
	for _, c := range oldCasks {
		if _, err := os.Stat(filepath.Join(mine, "Caskroom", c.token, c.version)); err == nil {
			t.Errorf("brw left %s %s behind", c.token, c.version)
		}
		meta := filepath.Join("Caskroom", c.token, ".metadata")
		a := caskMetadata(t, filepath.Join(theirs, meta), theirs, theirApps, home)
		b := caskMetadata(t, filepath.Join(mine, meta), mine, myApps, filepath.Join(work, "home"), home)
		for name := range a {
			if !reflect.DeepEqual(a[name], b[name]) {
				t.Errorf("%s %s:\nbrew: %v\nbrw:  %v", c.token, name, a[name], b[name])
			}
		}
	}

	// Nothing is left to upgrade, as either tool sees brw's prefix.
	if got := brwIn("outdated", "--cask", "--greedy"); got != "" {
		t.Errorf("brw outdated after brw upgrade:\n%s", got)
	}
	if got := brewIn(mine, "outdated", "--cask", "--greedy"); got != "" {
		t.Errorf("brew outdated after brw upgrade:\n%s", got)
	}
	if got := brwIn("upgrade"); got != "Nothing to upgrade.\n" {
		t.Errorf("brw upgrade again:\n%s", got)
	}
	if got, want := brewIn(mine, "list", "--cask", "--versions"), brewIn(theirs, "list", "--cask", "--versions"); got != want {
		t.Errorf("brew list --cask --versions on brw's prefix:\n%s\nbrew's:\n%s", got, want)
	}
}

// fetchOldCask downloads a cask file as it was at a homebrew-cask commit.
func fetchOldCask(t *testing.T, commit, path string) []byte {
	t.Helper()
	resp, err := http.Get("https://raw.githubusercontent.com/Homebrew/homebrew-cask/" + commit + "/" + path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("%s at %s: %s %v", path, commit, resp.Status, err)
	}
	return b
}
