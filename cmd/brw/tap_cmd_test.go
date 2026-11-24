package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/tap"
	"github.com/jigsmiths/brw/internal/trust"
)

func tapCmdEnv(t *testing.T) (e *env, prefix, origin string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOMEBREW_CACHE", t.TempDir())
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "")
	for _, v := range []string{"HOMEBREW_ALLOWED_TAPS", "HOMEBREW_FORBIDDEN_TAPS", "HOMEBREW_FORBIDDEN_OWNER", "HOMEBREW_FORBIDDEN_OWNER_CONTACT"} {
		t.Setenv(v, "")
	}
	origin = t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	writeTapFormula(t, origin, "tool", toolFormula("Tool", "1.0"))
	return &env{err: &bytes.Buffer{}, host: platform.Current(macVersion())}, t.TempDir(), origin
}

func stderrOf(e *env) string { return e.err.(*bytes.Buffer).String() }

func tapDir(prefix, name string) string { return tap.Dir(prefix, name) }

func remoteOf(prefix, name string) string {
	return (tap.Tap{Name: name, Dir: tapDir(prefix, name)}).Remote()
}

func mkdirs(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func cloneForTest(t *testing.T, prefix, name, origin string) {
	t.Helper()
	if _, err := tap.Clone(t.Context(), prefix, name, origin); err != nil {
		t.Fatal(err)
	}
}

func TestTapClonesIntoThePrefixFromAnyURL(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	out, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin)
	if err != nil {
		t.Fatalf("tap: %v", err)
	}
	if !pathExists(filepath.Join(tapDir(prefix, "u/t"), "Formula", "tool.rb")) {
		t.Errorf("tap not cloned")
	}
	if w := stderrOf(e); !strings.Contains(w, "==> Tapping u/t") || !strings.Contains(w, "Tapped 1 formula (") {
		t.Errorf("stderr = %q", w)
	}
	if !strings.Contains(out, "Tap u/t is not trusted. Run `brw trust u/t` to trust it.") {
		t.Errorf("stdout = %q", out)
	}
	if store, _ := trust.Load(trust.Path()); len(store.Entries(trust.KindTap)) != 0 {
		t.Errorf("tap was trusted automatically")
	}
}

func TestTapSaysWhenTheCloneIsTrusted(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	store, _ := trust.Load(trust.Path())
	if _, err := store.Trust(trust.KindTap, strings.ToLower(origin)); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin)
	if err != nil || !strings.Contains(out, "Tap u/t is trusted.") {
		t.Errorf("out = %q, %v", out, err)
	}
}

func TestTapListsSortedTaps(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	for _, name := range []string{"zed/t", "abe/t", "abe/a"} {
		cloneForTest(t, prefix, name, origin)
	}
	out, err := runCmd(t, e, "tap", "-prefix", prefix)
	if err != nil || out != "abe/a\nabe/t\nzed/t\n" {
		t.Errorf("tap = %q, %v", out, err)
	}
}

func TestTapOfAnAlreadyTappedRepoSaysNothing(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	cloneForTest(t, prefix, "u/t", origin)
	for _, args := range [][]string{{"u/t"}, {"u/t", origin}} {
		out, err := runCmd(t, e, "tap", append([]string{"-prefix", prefix}, args...)...)
		if err != nil || out != "" || stderrOf(e) != "" {
			t.Errorf("tap %v: out = %q, err = %v, stderr = %q", args, out, err, stderrOf(e))
		}
	}
}

func TestTapOfADifferentRemoteIsAMismatch(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	cloneForTest(t, prefix, "u/t", origin)
	other := t.TempDir()
	gitIn(t, other, "init", "-q", "-b", "main")
	for _, flags := range [][]string{nil, {"--force"}} {
		args := append(append([]string{"-prefix", prefix}, flags...), "u/t", other)
		_, err := runCmd(t, e, "tap", args...)
		if want := "Tap u/t remote mismatch.\n" + origin + " != " + other; err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("tap %v: err = %v, want %q", flags, err, want)
		}
	}
	if got := remoteOf(prefix, "u/t"); got != origin {
		t.Errorf("remote = %q after a refused tap", got)
	}
}

func TestTapComparesRemotesAsBrewDoes(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	cloneForTest(t, prefix, "u/t", origin)
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin+"/"); err == nil {
		t.Errorf("a remote differing by a trailing slash was accepted")
	}
}

func TestTapCustomRemoteChangesTheRemoteAndFetches(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	cloneForTest(t, prefix, "u/t", origin)
	other := t.TempDir()
	gitIn(t, other, "init", "-q", "-b", "main")
	writeTapFormula(t, other, "second", toolFormula("Second", "2.0"))
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "--custom-remote", "u/t", other); err != nil {
		t.Fatal(err)
	}
	if got := remoteOf(prefix, "u/t"); got != other {
		t.Errorf("remote = %q, want %q", got, other)
	}
	if w := stderrOf(e); !strings.Contains(w, "u/t: changed remote from "+origin+" to "+other) {
		t.Errorf("stderr = %q", w)
	}
	revParse := func(dir, ref string) []byte {
		cmd := exec.Command("git", "-C", dir, "rev-parse", ref)
		cmd.Env = tap.GitEnv()
		out, _ := cmd.Output()
		return out
	}
	want := revParse(other, "HEAD")
	got := revParse(tapDir(prefix, "u/t"), "refs/remotes/origin/main")
	if len(want) == 0 || string(got) != string(want) {
		t.Errorf("origin/main = %q, want the new remote's %q", got, want)
	}
}

func TestTapCustomRemoteNeedsAURL(t *testing.T) {
	e, prefix, _ := tapCmdEnv(t)
	_, err := runCmd(t, e, "tap", "-prefix", prefix, "--custom-remote", "u/t")
	if err == nil || !strings.Contains(err.Error(), "Tap u/t with option `--custom-remote` but without a remote URL.") {
		t.Errorf("err = %v", err)
	}
}

func TestTapUnshallowsAShallowTap(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	writeTapFormula(t, origin, "second", toolFormula("Second", "2.0"))
	remote := "file://" + origin
	mkdirs(t, filepath.Dir(tapDir(prefix, "u/t")))
	gitIn(t, filepath.Dir(tapDir(prefix, "u/t")), "clone", "-q", "--depth=1", remote, tapDir(prefix, "u/t"))
	if !pathExists(filepath.Join(tapDir(prefix, "u/t"), ".git", "shallow")) {
		t.Skip("git made a full clone")
	}
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", remote); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(tapDir(prefix, "u/t"), ".git", "shallow")) {
		t.Errorf("tap still shallow")
	}
	if w := stderrOf(e); !strings.Contains(w, "Unshallowing u/t") {
		t.Errorf("stderr = %q", w)
	}
}

func TestTapNamesFollowBrew(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "U/homebrew-T", origin); err != nil {
		t.Fatal(err)
	}
	if !pathExists(tapDir(prefix, "u/t")) {
		t.Errorf("U/homebrew-T was not tapped as u/t")
	}
	for _, bad := range []string{"nope", "a/b/c", "u/.", "/t", "u/"} {
		if _, err := runCmd(t, e, "tap", "-prefix", prefix, bad); err == nil || !strings.Contains(err.Error(), "Invalid tap name: '"+bad+"'") {
			t.Errorf("tap %s: err = %v", bad, err)
		}
	}
}

func TestTapRefusesHomebrewCoreUnderAnyName(t *testing.T) {
	e, prefix, _ := tapCmdEnv(t)
	for _, name := range []string{"homebrew/core", "Homebrew/homebrew-core", "linuxbrew/core", "homebrew/homebrew"} {
		_, err := runCmd(t, e, "tap", "-prefix", prefix, "--force", name)
		if err == nil || !strings.Contains(err.Error(), "unlike brew, brw reads core from the API") {
			t.Errorf("tap %s: err = %v", name, err)
		}
	}
}

func TestTapOfHomebrewCaskNeedsForce(t *testing.T) {
	e, prefix, _ := tapCmdEnv(t)
	_, err := runCmd(t, e, "tap", "-prefix", prefix, "homebrew/cask")
	if err == nil || !strings.Contains(err.Error(), "Tapping homebrew/cask is no longer typically necessary.\nAdd --force if you are sure you need it for contributing to Homebrew.") {
		t.Errorf("err = %v", err)
	}
}

func TestTapRefusesDeprecatedAndMovedTaps(t *testing.T) {
	e, prefix, _ := tapCmdEnv(t)
	for name, want := range map[string]string{
		"homebrew/cask-fonts": "homebrew/cask-fonts was deprecated. This tap is now empty and all its contents were either deleted or migrated.",
		"caskroom/cask":       "caskroom/cask was moved. Tap homebrew/cask instead.",
		"caskroom/versions":   "caskroom/versions was moved. Tap homebrew/cask-versions instead.",
		"phinze/cask":         "phinze/cask was moved. Tap homebrew/cask instead.",
	} {
		if _, err := runCmd(t, e, "tap", "-prefix", prefix, name); err == nil || err.Error() != want {
			t.Errorf("tap %s: err = %v, want %q", name, err, want)
		}
	}
}

func TestTapHonoursAllowedAndForbiddenTaps(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	t.Setenv("HOMEBREW_ALLOWED_TAPS", "other/x")
	_, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin)
	want := "The installation of the u/homebrew-t was requested but you\nhas not allowed this tap in `$HOMEBREW_ALLOWED_TAPS`."
	if err == nil || err.Error() != want {
		t.Errorf("allowed: err = %v, want %q", err, want)
	}
	t.Setenv("HOMEBREW_ALLOWED_TAPS", "u/t "+origin)
	t.Setenv("HOMEBREW_FORBIDDEN_TAPS", "u/t")
	t.Setenv("HOMEBREW_FORBIDDEN_OWNER", "Acme IT")
	t.Setenv("HOMEBREW_FORBIDDEN_OWNER_CONTACT", "it@acme.example")
	if _, err = runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin); err != nil {
		t.Errorf("a tap on a custom remote matched a name reference: %v", err)
	}
	t.Setenv("HOMEBREW_ALLOWED_TAPS", "other/x")
	t.Setenv("HOMEBREW_FORBIDDEN_TAPS", origin)
	_, err = runCmd(t, e, "tap", "-prefix", prefix, "v/t", origin)
	want = "The installation of the v/homebrew-t was requested but Acme IT\nhas not allowed this tap in `$HOMEBREW_ALLOWED_TAPS` and\nhas forbidden this tap in `$HOMEBREW_FORBIDDEN_TAPS`.\nit@acme.example"
	if err == nil || err.Error() != want {
		t.Errorf("forbidden: err = %v, want %q", err, want)
	}
	if pathExists(filepath.Join(prefix, "Library", "Taps", "v")) {
		t.Errorf("a forbidden tap was cloned")
	}
}

func TestTapNeedsGit(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	t.Setenv("PATH", t.TempDir())
	_, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin)
	if err == nil || !strings.Contains(err.Error(), "git") || !strings.Contains(err.Error(), "unlike brew") {
		t.Errorf("err = %v", err)
	}
}

func TestTapCleansUpAFailedClone(t *testing.T) {
	e, prefix, _ := tapCmdEnv(t)
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("tap of a missing remote succeeded")
	}
	if pathExists(filepath.Join(prefix, "Library", "Taps", "u")) {
		t.Errorf("left a partial tap behind")
	}
}

func TestTapDryRunCreatesNothing(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	out, err := runCmd(t, e, "tap", "-prefix", prefix, "--dry-run", "u/t", origin)
	if err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(prefix, "Library")) {
		t.Errorf("dry run created Library")
	}
	if !strings.Contains(out, "would clone") {
		t.Errorf("out = %q", out)
	}
}

func TestTapDryRunChangesNoRemote(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	cloneForTest(t, prefix, "u/t", origin)
	other := t.TempDir()
	out, err := runCmd(t, e, "tap", "-prefix", prefix, "--dry-run", "--custom-remote", "u/t", other)
	if err != nil || !strings.Contains(out, "would change the remote of u/t from "+origin+" to "+other) {
		t.Errorf("out = %q, %v", out, err)
	}
	if got := remoteOf(prefix, "u/t"); got != origin {
		t.Errorf("dry run changed the remote to %q", got)
	}
}

func TestTapJournalsItsChanges(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	journal := filepath.Join(t.TempDir(), "journal")
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "-journal", journal, "u/t", origin); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(journal); !strings.Contains(string(b), "mkdir") {
		t.Errorf("journal = %q", b)
	}
}

func TestTapLinksManpagesAndUntapUnlinksThem(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	writeFile(t, filepath.Join(origin, "manpages", "brew-x.1"), ".TH x\n")
	gitIn(t, origin, "add", "-A")
	gitIn(t, origin, "commit", "-q", "-m", "man")
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(prefix, "share", "man", "man1", "brew-x.1")
	if dest, err := os.Readlink(link); err != nil || dest != "../../../Library/Taps/u/homebrew-t/manpages/brew-x.1" {
		t.Fatalf("link = %q, %v", dest, err)
	}
	if _, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t"); err != nil {
		t.Fatal(err)
	}
	if pathExists(link) {
		t.Errorf("man page link left behind")
	}
}

func TestTapKeepsAForeignManpageLink(t *testing.T) {
	e, prefix, origin := tapCmdEnv(t)
	writeFile(t, filepath.Join(origin, "manpages", "brew-x.1"), ".TH x\n")
	gitIn(t, origin, "add", "-A")
	gitIn(t, origin, "commit", "-q", "-m", "man")
	mkdirs(t, filepath.Join(prefix, "share", "man", "man1"))
	writeFile(t, filepath.Join(prefix, "share", "man", "man1", "brew-x.1"), "mine")
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "share", "man", "man1", "brew-x.1")); string(b) != "mine" {
		t.Errorf("replaced a file that was not a link")
	}
}

func untapSetup(t *testing.T) (e *env, prefix string) {
	e, prefix, origin := tapCmdEnv(t)
	if _, err := runCmd(t, e, "tap", "-prefix", prefix, "u/t", origin); err != nil {
		t.Fatal(err)
	}
	e.err = &bytes.Buffer{}
	return e, prefix
}

func TestUntapRemovesTheTapAndItsEmptyUserDir(t *testing.T) {
	e, prefix := untapSetup(t)
	if _, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t"); err != nil {
		t.Fatal(err)
	}
	if w := stderrOf(e); !strings.Contains(w, "Untapping u/t...") || !strings.Contains(w, "Untapped 1 formula (") {
		t.Errorf("stderr = %q", w)
	}
	if pathExists(filepath.Join(prefix, "Library", "Taps", "u")) {
		t.Errorf("user dir left behind")
	}
	if !pathExists(filepath.Join(prefix, "Library", "Taps")) {
		t.Errorf("Taps removed")
	}
}

func TestUntapKeepsAUserDirThatHasOtherTaps(t *testing.T) {
	e, prefix := untapSetup(t)
	mkdirs(t, tapDir(prefix, "u/other"))
	if _, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t"); err != nil {
		t.Fatal(err)
	}
	if !pathExists(tapDir(prefix, "u/other")) {
		t.Errorf("removed a sibling tap")
	}
}

func TestUntapOfAnUnknownTapChangesNothing(t *testing.T) {
	e, prefix := untapSetup(t)
	_, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t", "x/y")
	if err == nil || !strings.Contains(err.Error(), "No available tap x/y.") {
		t.Errorf("err = %v", err)
	}
	if !pathExists(tapDir(prefix, "u/t")) {
		t.Errorf("untapped u/t although x/y was unknown")
	}
}

func TestUntapRefusesHomebrewCore(t *testing.T) {
	e, prefix, _ := tapCmdEnv(t)
	writeFile(t, filepath.Join(tapDir(prefix, "homebrew/core"), "x"), "")
	_, err := runCmd(t, e, "untap", "-prefix", prefix, "homebrew/core")
	if err == nil || !strings.Contains(err.Error(), "unlike brew, brw reads core from the API") {
		t.Errorf("err = %v", err)
	}
	if !pathExists(tapDir(prefix, "homebrew/core")) {
		t.Errorf("core removed")
	}
}

func installedTapPackages(t *testing.T, prefix string) {
	t.Helper()
	mkdirs(t, filepath.Join(prefix, "opt"))
	tapKeg(t, prefix, "tool", "1.0", "u/t")
	writeFile(t, filepath.Join(prefix, "Caskroom", "app", ".metadata", "1.0", "20240101000000.000", "Casks", "app.rb"), "cask \"app\" do\n  version \"1.0\"\nend\n")
	writeFile(t, filepath.Join(prefix, "Caskroom", "app", ".metadata", "INSTALL_RECEIPT.json"), `{"source":{"tap":"u/t"},"uninstall_artifacts":[{"uninstall":[{}]}]}`)
}

func TestUntapRefusesATapWithInstalledPackagesWhenItCannotAsk(t *testing.T) {
	e, prefix := untapSetup(t)
	installedTapPackages(t, prefix)
	out, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t")
	if err == nil || !strings.Contains(err.Error(), "Refusing to untap u/t because it contains the following installed formulae and casks:\nu/t/tool\nu/t/app") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(out, "Would untap u/t after uninstalling the following formulae and casks:") {
		t.Errorf("out = %q", out)
	}
	if !pathExists(tapDir(prefix, "u/t")) || !pathExists(filepath.Join(prefix, "Cellar", "tool")) {
		t.Errorf("something was removed despite the refusal")
	}
}

func TestUntapAsksBeforeUninstalling(t *testing.T) {
	for _, answer := range []bool{false, true} {
		e, prefix := untapSetup(t)
		installedTapPackages(t, prefix)
		var asked []string
		e.confirm = func(action string) bool { asked = append(asked, action); return answer }
		_, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t")
		if len(asked) != 1 || asked[0] != "changes" {
			t.Errorf("asked = %v", asked)
		}
		if answer != (err == nil) {
			t.Errorf("answer %v: err = %v", answer, err)
		}
		if answer == pathExists(tapDir(prefix, "u/t")) || answer == pathExists(filepath.Join(prefix, "Cellar", "tool", "1.0")) ||
			answer == pathExists(filepath.Join(prefix, "Caskroom", "app")) {
			t.Errorf("answer %v: tap, keg or cask in the wrong state", answer)
		}
	}
}

func TestUntapForceUninstallsWithoutAsking(t *testing.T) {
	e, prefix := untapSetup(t)
	installedTapPackages(t, prefix)
	e.confirm = func(string) bool { t.Error("asked despite --force"); return false }
	if _, err := runCmd(t, e, "untap", "-prefix", prefix, "--force", "u/t"); err != nil {
		t.Fatal(err)
	}
	if pathExists(tapDir(prefix, "u/t")) || pathExists(filepath.Join(prefix, "Cellar", "tool", "1.0")) {
		t.Errorf("tap or keg kept after --force")
	}
}

func TestUntapIgnoresPackagesFromOtherTaps(t *testing.T) {
	e, prefix := untapSetup(t)
	mkdirs(t, filepath.Join(prefix, "opt"))
	tapKeg(t, prefix, "other", "1.0", "x/y")
	if _, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t"); err != nil {
		t.Errorf("err = %v", err)
	}
	if !pathExists(filepath.Join(prefix, "Cellar", "other", "1.0")) {
		t.Errorf("removed a keg from another tap")
	}
}

func TestUntapClearsTheTapsTrustEntries(t *testing.T) {
	e, prefix := untapSetup(t)
	ref := trust.Reference("u/t", remoteOf(prefix, "u/t"))
	store, _ := trust.Load(trust.Path())
	for kind, entry := range map[trust.Kind]string{
		trust.KindTap: ref, trust.KindFormula: ref + "/tool", trust.KindCask: ref + "/app",
	} {
		if _, err := store.Trust(kind, entry); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Trust(trust.KindTap, "x/y"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Trust(trust.KindFormula, "x/y/z"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t"); err != nil {
		t.Fatal(err)
	}
	store, _ = trust.Load(trust.Path())
	if got := store.Entries(trust.KindTap); len(got) != 1 || got[0] != "x/y" {
		t.Errorf("taps = %v", got)
	}
	if got := store.Entries(trust.KindFormula); len(got) != 1 || got[0] != "x/y/z" {
		t.Errorf("formulae = %v", got)
	}
	if got := store.Entries(trust.KindCask); len(got) != 0 {
		t.Errorf("casks = %v", got)
	}
}

func TestUntapDropsTheTapsDescriptionCacheEntries(t *testing.T) {
	e, prefix := untapSetup(t)
	cache := os.Getenv("HOMEBREW_CACHE")
	writeFile(t, filepath.Join(cache, "descriptions.json"), `{"a/b/keep":["a","b"],"u/t/tool":["x","y"]}`)
	writeFile(t, filepath.Join(cache, "cask_descriptions.json"), `{"u/t/app":["App","d"],"zed":["Z","z"]}`)
	if _, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t"); err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		"descriptions.json":      `{"a/b/keep":["a","b"]}`,
		"cask_descriptions.json": `{"zed":["Z","z"]}`,
	} {
		if b, _ := os.ReadFile(filepath.Join(cache, file)); string(b) != want {
			t.Errorf("%s = %s, want %s", file, b, want)
		}
	}
}

func TestUntapWithoutADescriptionCacheIsFine(t *testing.T) {
	e, prefix := untapSetup(t)
	if _, err := runCmd(t, e, "untap", "-prefix", prefix, "u/t"); err != nil {
		t.Fatal(err)
	}
	if pathExists(filepath.Join(os.Getenv("HOMEBREW_CACHE"), "descriptions.json")) {
		t.Errorf("created a description cache")
	}
}

func TestUntapDryRunChangesNothingAndShowsThePlan(t *testing.T) {
	e, prefix := untapSetup(t)
	installedTapPackages(t, prefix)
	cache := os.Getenv("HOMEBREW_CACHE")
	writeFile(t, filepath.Join(cache, "descriptions.json"), `{"u/t/tool":["x","y"]}`)
	if _, err := trustStore(t).Trust(trust.KindTap, trust.Reference("u/t", remoteOf(prefix, "u/t"))); err != nil {
		t.Fatal(err)
	}
	e.confirm = func(string) bool { t.Error("asked in a dry run"); return false }
	out, err := runCmd(t, e, "untap", "-prefix", prefix, "--dry-run", "u/t")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Would untap u/t after uninstalling", "Would uninstall ", "Cellar/tool/1.0", "==> Would uninstall 1 cask", "removeall", "would untrust"} {
		if !strings.Contains(out, want) {
			t.Errorf("out lacks %q:\n%s", want, out)
		}
	}
	if !pathExists(tapDir(prefix, "u/t")) || !pathExists(filepath.Join(prefix, "Cellar", "tool", "1.0")) {
		t.Errorf("dry run removed something")
	}
	if b, _ := os.ReadFile(filepath.Join(cache, "descriptions.json")); string(b) != `{"u/t/tool":["x","y"]}` {
		t.Errorf("dry run changed the description cache: %s", b)
	}
	if len(trustStore(t).Entries(trust.KindTap)) != 1 {
		t.Errorf("dry run changed trust")
	}
}

func trustStore(t *testing.T) *trust.Store {
	t.Helper()
	s, err := trust.Load(trust.Path())
	if err != nil {
		t.Fatal(err)
	}
	return s
}
