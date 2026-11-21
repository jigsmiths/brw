package main

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/tap"
)

// This test installs an old brw from a copy of jigsmiths/tap with brew and
// with brw, releases a newer one to the copy, and upgrades each prefix with
// its own tool after updating the taps. It needs the checkout described in
// conformance_cask_test.go and BRW_CONFORMANCE_TAP=1.

// brwReleases are brw's cask file at two of jigsmiths/homebrew-tap's commits.
var brwReleases = []struct{ commit, version string }{
	{"b96f532e37a8e0ebf45cecdadc6339194adecc4e", "0.18.0"},
	{"11cc5665db54dccf2d6e9c220bc5e10627b068e9", "0.19.0"},
}

func TestConformanceTapCask(t *testing.T) {
	if os.Getenv("BRW_CONFORMANCE_TAP") == "" {
		t.Skip("set BRW_CONFORMANCE_TAP=1 to compare installing and upgrading a tap's cask with brew")
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
	// Each tool keeps its trust store in a scratch config folder, not the user's.
	theirConfig, myConfig := filepath.Join(work, "config-brew"), filepath.Join(work, "config-brw")
	common := []string{"HOMEBREW_NO_AUTO_UPDATE=1", "HOMEBREW_NO_ENV_HINTS=1", "HOMEBREW_NO_ANALYTICS=1", "HOMEBREW_CACHE=" + cache,
		"HOMEBREW_NO_REQUIRE_TAP_TRUST="}
	env := append(slices.Clone(common), "XDG_CONFIG_HOME="+theirConfig)
	brwEnv := append(slices.Clone(common), "HOME="+filepath.Join(work, "home"), "XDG_CONFIG_HOME="+myConfig)
	// brew sets up its own Ruby the first time it needs it, whatever the command.
	rubySetup := regexp.MustCompile(`(?m)^(==> Downloading \S+/portable-ruby/|Already downloaded: \S+/portable-ruby-|==> Pouring portable-ruby-).*\n`)
	brewIn := func(prefix string, args ...string) string {
		return rubySetup.ReplaceAllString(sh(t, "", env, filepath.Join(prefix, "bin", "brew"), args...), "")
	}
	brwIn := func(args ...string) string {
		return sh(t, "", brwEnv, bin, append(args[:1:1], append([]string{"-prefix", mine}, args[1:]...)...)...)
	}
	brewIn(theirs, "update", "--quiet")
	brewIn(mine, "update", "--quiet")

	// origin stands in for github.com/jigsmiths/homebrew-tap. Each release
	// commits the cask file, then the listing brew generates from it, as the
	// tap's workflow does. brew's prefix is tapped from origin and follows it.
	origin := filepath.Join(work, "origin")
	theirTap := tap.Dir(theirs, "jigsmiths/tap")
	commit := func(msg string) {
		sh(t, origin, nil, "git", "add", "-A")
		sh(t, origin, nil, "git", "-c", "user.name=brw", "-c", "user.email=brw@example.com", "-c", "commit.gpgsign=false", "commit", "-qm", msg)
	}
	follow := func() {
		if _, err := os.Stat(theirTap); err != nil {
			brewIn(theirs, "tap", "jigsmiths/tap", origin)
			return
		}
		sh(t, theirTap, nil, "git", "pull", "-q", "--ff-only")
	}
	release := func(commitID, version string) {
		writeTapFile(t, origin, "Casks/brw.rb", fetchTapFile(t, commitID, "Casks/brw.rb"))
		commit("brw " + version)
		follow()
		// The tap's workflow trusts the tap; here that would also trust it for
		// the comparison, so the listing is made without asking for trust.
		cmd := osexec.Command(filepath.Join(theirs, "bin", "brew"), "info", "--json=v2", "--variations", "--cask", "jigsmiths/tap/brw")
		cmd.Env = append(append(os.Environ(), env...), "HOMEBREW_NO_REQUIRE_TAP_TRUST=1")
		var stderr strings.Builder
		cmd.Stderr = &stderr
		gen, err := cmd.Output()
		var info struct{ Casks []json.RawMessage }
		if err != nil || json.Unmarshal(gen, &info) != nil {
			t.Fatalf("brew info: %v\n%s\n%s", err, gen, stderr.String())
		}
		listing, err := json.Marshal(info.Casks)
		if err != nil {
			t.Fatal(err)
		}
		writeTapFile(t, origin, "api/cask.json", listing)
		commit("listing for brw " + version)
		follow()
	}
	sh(t, "", nil, "git", "init", "-q", "-b", "main", origin)
	release(brwReleases[0].commit, brwReleases[0].version)

	// brw finds the tap there, cloned as it would clone it.
	if _, err := tap.Clone(t.Context(), mine, "jigsmiths/tap", origin); err != nil {
		t.Fatal(err)
	}
	brewIn(theirs, "install", "--cask", "--appdir", theirApps, "jigsmiths/tap/brw")
	brwIn("install", "--cask", "--appdir", myApps, "jigsmiths/tap/brw")
	// Naming the cask trusted it, and both stores say so the same way.
	a, errA := os.ReadFile(filepath.Join(theirConfig, "homebrew", "trust.json"))
	b, errB := os.ReadFile(filepath.Join(myConfig, "homebrew", "trust.json"))
	if errA != nil || errB != nil || string(a) != string(b) {
		t.Errorf("trust stores differ:\nbrew (%v):\n%s\nbrw (%v):\n%s", errA, a, errB, b)
	}

	stamp := regexp.MustCompile(`(?m)/\d{14}\.\d{3}(/|$)`)
	normal := func(s, prefix, apps string) string {
		s = strings.ReplaceAll(s, prefix, "<prefix>")
		s = strings.ReplaceAll(s, apps, "<apps>")
		return stamp.ReplaceAllString(s, "/<time>$1")
	}
	compare := func(step string) {
		t.Helper()
		if d := lineDiff(normal(snapshot(t, theirs), theirs, theirApps), normal(snapshot(t, mine), mine, myApps)); len(d) > 0 {
			t.Errorf("%s: prefixes differ:\n%s", step, strings.Join(d, "\n"))
		}
		meta := filepath.Join("Caskroom", "brw", ".metadata")
		a := caskMetadata(t, filepath.Join(theirs, meta), theirs, theirApps, home)
		b := caskMetadata(t, filepath.Join(mine, meta), mine, myApps, filepath.Join(work, "home"), home)
		for name := range a {
			if !reflect.DeepEqual(a[name], b[name]) {
				t.Errorf("%s: brw %s:\nbrew: %v\nbrw:  %v", step, name, a[name], b[name])
			}
		}
		// The taps are at the same commit, so the receipts name the same one.
		if ha, hb := tapGitHead(t, filepath.Join(theirs, meta)), tapGitHead(t, filepath.Join(mine, meta)); ha == "" || ha != hb {
			t.Errorf("%s: tap_git_head: brew %q, brw %q", step, ha, hb)
		}
	}
	compare("install")

	release(brwReleases[1].commit, brwReleases[1].version)
	if out := brwIn("update"); !strings.Contains(out, "Updated 1 tap (jigsmiths/tap).\n") {
		t.Errorf("brw update did not pull the tap:\n%s", out)
	}
	if a, b := sh(t, theirTap, nil, "git", "rev-parse", "HEAD"), sh(t, tap.Dir(mine, "jigsmiths/tap"), nil, "git", "rev-parse", "HEAD"); a != b {
		t.Errorf("tap heads differ: brew %s, brw %s", a, b)
	}
	if a, b := brewIn(theirs, "outdated", "--cask", "--quiet"), brwIn("outdated", "--cask", "--quiet"); a != b || a != "brw\n" {
		t.Errorf("outdated: brew %q, brw %q", a, b)
	}

	steps := regexp.MustCompile(`(?m)^(==> |  \S+ -> |\S+ +\S+ +-> |.*successfully upgraded|Warning|Error).*$`)
	upgrade := func(out, prefix, apps string) string {
		var keep []string
		for _, l := range steps.FindAllString(normal(out, prefix, apps), -1) {
			if l != "==> Cleanup" { // of brew's own download cache
				keep = append(keep, l)
			}
		}
		return strings.Join(keep, "\n")
	}
	ua := upgrade(brewIn(theirs, "upgrade", "--cask", "--no-ask"), theirs, theirApps)
	ub := upgrade(brwIn("upgrade", "--cask"), mine, myApps)
	if d := lineDiff(ua, ub); len(d) > 0 {
		t.Errorf("upgrade output differs:\n%s\nbrew:\n%s\nbrw:\n%s", strings.Join(d, "\n"), ua, ub)
	}
	compare("upgrade")
	if _, err := os.Stat(filepath.Join(mine, "Caskroom", "brw", brwReleases[1].version)); err != nil {
		t.Errorf("brw did not install %s: %v", brwReleases[1].version, err)
	}

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

func tapGitHead(t *testing.T, meta string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(meta, "INSTALL_RECEIPT.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Source struct {
			TapGitHead string `json:"tap_git_head"`
		} `json:"source"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r.Source.TapGitHead
}

func writeTapFile(t *testing.T, repo, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, path)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, path), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// fetchTapFile downloads a file as it was at a jigsmiths/homebrew-tap commit.
func fetchTapFile(t *testing.T, commit, path string) []byte {
	t.Helper()
	resp, err := http.Get("https://raw.githubusercontent.com/jigsmiths/homebrew-tap/" + commit + "/" + path)
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
