package main

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jigsmiths/brw/internal/tap"
)

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Env = tap.GitEnv() // in the pre-commit hook, git would otherwise use brw's index
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func publishTapCask(t *testing.T, repo string, c map[string]any, version string) {
	t.Helper()
	writeTapCask(t, repo, c["token"].(string), fmt.Sprintf(`cask %q do
  version %q
  sha256 %q

  url %q
  name "App"

  app "App.app"
  binary "tool"
end
`, c["token"], version, c["sha256"], c["url"]))
	gitIn(t, repo, "commit", "-q", "-m", c["token"].(string)+" "+version)
}

func writeTapCask(t *testing.T, repo, token, rb string) {
	t.Helper()
	path := filepath.Join(repo, "Casks", token+".rb")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(rb), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "add", "-A")
}

func tapStore(t *testing.T) (e *env, prefix, origin string, app map[string]any) {
	t.Helper()
	// brew's trust store goes here, never the user's.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "")
	s := newStore(t)
	s.appCask("app")
	app, s.casks = s.casks[0], nil
	e, prefix = s.env()
	origin = t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	publishTapCask(t, origin, app, "1.0")
	e.tapRemote = func(name string) string {
		if name != "jigsmiths/tap" {
			return filepath.Join(t.TempDir(), "missing")
		}
		return origin
	}
	return e, prefix, origin, app
}

func TestInstallUpdateAndUpgradeACaskFromATap(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	e, prefix, origin, app := tapStore(t)
	apps := t.TempDir()

	out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "jigsmiths/tap/app")
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if !strings.Contains(out, "app was successfully installed!\n") {
		t.Errorf("install output:\n%s", out)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.HasPrefix(w, "==> Tapping jigsmiths/tap\n==> Trusted cask jigsmiths/tap/app\n") {
		t.Errorf("stderr = %q", w)
	}
	// Tapped from elsewhere than GitHub, the tap is named by its remote.
	if got := trustFile(t); !strings.Contains(got, `"`+strings.ToLower(origin)+`/app"`) {
		t.Errorf("trust.json:\n%s", got)
	}
	receipt, err := os.ReadFile(filepath.Join(prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(prefix, "Library/Taps/jigsmiths/homebrew-tap/Casks/app.rb")
	if !strings.Contains(string(receipt), `"loaded_from_api": false`) || !strings.Contains(string(receipt), `"tap": "jigsmiths/tap"`) ||
		!strings.Contains(string(receipt), `"path": "`+wantPath+`"`) {
		t.Errorf("receipt:\n%s", receipt)
	}

	publishTapCask(t, origin, app, "2.0")
	out, err = runCmd(t, e, "update", "-prefix", prefix)
	if err != nil || !strings.HasSuffix(out, "Updated 1 tap (jigsmiths/tap).\n") {
		t.Errorf("update = %q, %v", out, err)
	}
	out, err = runCmd(t, e, "upgrade", "-prefix", prefix)
	// brew names a tap's cask in full when fetching, and by token after.
	if err != nil || !strings.Contains(out, "==> Fetching downloads for: jigsmiths/tap/app\n==> Upgrading app\n  1.0 -> 2.0\n") ||
		!strings.Contains(out, "app was successfully upgraded!\n") {
		t.Fatalf("upgrade = %q, %v\n%s", out, err, e.err)
	}
	if out, err := runCmd(t, e, "upgrade", "-prefix", prefix); err != nil || out != "Nothing to upgrade.\n" {
		t.Errorf("upgrade again = %q, %v", out, err)
	}
	if out, err := runCmd(t, e, "update", "-prefix", prefix); err != nil || strings.Contains(out, "tap") {
		t.Errorf("update with nothing new = %q, %v", out, err)
	}
}

func trustFile(t *testing.T) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "homebrew", "trust.json"))
	return string(b)
}

func TestCasksOfAnUntrustedTapAreSkipped(t *testing.T) {
	e, prefix, origin, _ := tapStore(t)
	if _, err := tap.Clone(t.Context(), prefix, "jigsmiths/tap", origin); err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	e.err = stderr
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "app"); err == nil {
		t.Error("installed a cask from an untrusted tap by its bare token")
	}
	if !strings.Contains(stderr.String(), "brw: warning: Skipping jigsmiths/tap because it is not trusted. Run `brw trust jigsmiths/tap` to trust it.\n") {
		t.Errorf("stderr = %q", stderr)
	}

	out, err := runCmd(t, e, "trust", "-prefix", prefix, "jigsmiths/tap")
	if want := "Trusted tap: " + strings.ToLower(origin) + "\n"; err != nil || out != want {
		t.Errorf("trust = %q, %v; want %q", out, err, want)
	}
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "app"); err != nil || !strings.Contains(out, "app was successfully installed!\n") {
		t.Errorf("install from the trusted tap = %q, %v", out, err)
	}

	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "1")
	if _, err := runCmd(t, e, "untrust", "-prefix", prefix, "jigsmiths/tap"); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if out, err := runCmd(t, e, "outdated", "-prefix", prefix); err != nil || strings.Contains(stderr.String(), "Skipping") {
		t.Errorf("with HOMEBREW_NO_REQUIRE_TAP_TRUST: %q, %v, %q", out, err, stderr)
	}
}

func TestTrustCommands(t *testing.T) {
	e, prefix, _, _ := tapStore(t)
	if out, err := runCmd(t, e, "trust"); err != nil || out != "All official taps and commands are trusted.\nNo trusted taps, formulae, casks or commands.\n" {
		t.Errorf("trust with nothing trusted = %q, %v", out, err)
	}
	for _, tc := range []struct{ args, want string }{
		{"trust homebrew/cask", "Official tap homebrew/cask is always trusted.\n"},
		{"trust someone/tap", "Trusted tap: someone/tap\n"},
		{"trust someone/tap", "Already trusted tap: someone/tap\n"},
		{"trust someone/tap/thing", "Trusted cask: someone/tap/thing\n"},
		{"trust --cask other/tap/app", "Trusted cask: other/tap/app\n"},
		{"untrust someone/tap", "Untrusted tap: someone/tap\n"},
		{"untrust someone/tap", "Not trusted tap: someone/tap\n"},
	} {
		args := strings.Fields(tc.args)
		out, err := runCmd(t, e, args[0], append([]string{"-prefix", prefix}, args[1:]...)...)
		if err != nil || out != tc.want {
			t.Errorf("%s = %q, %v; want %q", tc.args, out, err, tc.want)
		}
	}
	// Untrusting a tap also drops its casks.
	if out, _ := runCmd(t, e, "trust"); out != "All official taps and commands are trusted.\nTrusted casks:\n  other/tap/app\n" {
		t.Errorf("trust listing = %q", out)
	}
	if _, err := runCmd(t, e, "trust", "-prefix", prefix, "a/b/c/d"); err == nil {
		t.Error("trusted a name that is neither a tap nor a cask")
	}
}

func TestInstallByFullNameUsesATapThatIsThere(t *testing.T) {
	e, prefix, _, _ := tapStore(t)
	apps := t.TempDir()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "--cask", "jigsmiths/tap/app"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(prefix, "Caskroom")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(apps, "App.app")); err != nil {
		t.Fatal(err)
	}
	e.tapRemote = func(string) string { return filepath.Join(t.TempDir(), "unreachable") }
	e.err = &bytes.Buffer{}
	out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "app")
	if err != nil || strings.Contains(e.err.(*bytes.Buffer).String(), "Tapping") || !strings.Contains(out, "app was successfully installed!\n") {
		t.Errorf("install from the tap there = %q, %v\n%s", out, err, e.err)
	}
}

func TestInstallByFullNameFromAnUnknownTap(t *testing.T) {
	e, prefix, _, _ := tapStore(t)
	stderr := &bytes.Buffer{}
	e.err = stderr
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "nobody/tap/app"); err == nil {
		t.Error("installed from a tap that cannot be cloned")
	}
	if !strings.Contains(stderr.String(), "nobody/tap") {
		t.Errorf("stderr = %q", stderr)
	}
	if exists(t, prefix, "Library/Taps/nobody") {
		t.Error("a failed tap left a folder behind")
	}
}

func TestInstallByFullNameSaysWhyACaskCannotBeRead(t *testing.T) {
	e, prefix, origin, _ := tapStore(t)
	writeTapCask(t, origin, "app", "cask \"app\" do\n  version \"1.0\"\n  postflight do\n    FileUtils.mv \"a\", \"b\"\n  end\nend\n")
	gitIn(t, origin, "commit", "-q", "-m", "ruby brw cannot read")
	stderr := &bytes.Buffer{}
	e.err = stderr
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "jigsmiths/tap/app"); err == nil {
		t.Fatal("installed a cask brw cannot read")
	}
	if w := stderr.String(); !strings.Contains(w, "jigsmiths/tap/app") || !strings.Contains(w, "FileUtils") {
		t.Errorf("stderr should name the cask and what brw cannot read: %q", w)
	}
}

func TestUpdateWarnsAboutATapItCannotPull(t *testing.T) {
	e, prefix, origin, _ := tapStore(t)
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "jigsmiths/tap/app"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	stderr := &bytes.Buffer{}
	e.err = stderr
	if _, err := runCmd(t, e, "update", "-prefix", prefix); err != nil {
		t.Errorf("update should carry on past a tap: %v", err)
	}
	if !strings.Contains(stderr.String(), "brw: warning: could not update jigsmiths/tap:") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestUpdateSkipsATapGitHubSaysIsCurrent(t *testing.T) {
	e, prefix, origin, app := tapStore(t)
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "jigsmiths/tap/app"); err != nil {
		t.Fatal(err)
	}
	// The tap says it is on GitHub, but git fetches it from origin.
	dir := tap.Dir(prefix, "jigsmiths/tap")
	gitIn(t, dir, "remote", "set-url", "origin", "https://github.com/jigsmiths/homebrew-tap")
	gitIn(t, dir, "config", "url."+origin+".insteadOf", "https://github.com/jigsmiths/homebrew-tap")
	publishTapCask(t, origin, app, "2.0")

	var current atomic.Bool
	current.Store(true)
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if current.Load() {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write([]byte("0123abcd"))
	}))
	defer github.Close()
	e.githubAPI = github.URL

	if out, err := runCmd(t, e, "update", "-prefix", prefix); err != nil || strings.Contains(out, "tap") {
		t.Errorf("update with GitHub saying the tap is current = %q, %v", out, err)
	}
	current.Store(false)
	if out, err := runCmd(t, e, "update", "-prefix", prefix); err != nil || !strings.HasSuffix(out, "Updated 1 tap (jigsmiths/tap).\n") {
		t.Errorf("update with GitHub ahead = %q, %v", out, err)
	}
}

func TestUpdateWarnsOnceAboutARejectedGitHubToken(t *testing.T) {
	t.Setenv("HOMEBREW_GITHUB_API_TOKEN", "expired")
	e, prefix, origin, _ := tapStore(t)
	for _, name := range []string{"jigsmiths/tap", "jigsmiths/other"} {
		if _, err := tap.Clone(t.Context(), prefix, name, origin); err != nil {
			t.Fatal(err)
		}
		gitIn(t, tap.Dir(prefix, name), "remote", "set-url", "origin", "https://github.com/jigsmiths/homebrew-tap")
	}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusNotModified)
	}))
	defer github.Close()
	e.githubAPI = github.URL
	e.err = &bytes.Buffer{}

	if _, err := runCmd(t, e, "update", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	want := "brw: warning: GitHub rejected HOMEBREW_GITHUB_API_TOKEN; checking taps without it\n"
	if w := e.err.(*bytes.Buffer).String(); w != want {
		t.Errorf("stderr = %q, want %q", w, want)
	}
}

func TestUpgradeListsUntrustedTapsItSkips(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	e, prefix, origin, app := tapStore(t)
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "jigsmiths/tap/app"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	other := t.TempDir()
	gitIn(t, other, "init", "-q", "-b", "main")
	publishTapCask(t, other, app, "1.0")
	if _, err := tap.Clone(t.Context(), prefix, "other/tap", other); err != nil {
		t.Fatal(err)
	}
	publishTapCask(t, origin, app, "2.0")
	if _, err := runCmd(t, e, "update", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	want := "==> Upgrading 1 outdated package:\napp 1.0 -> 2.0\n==> Skipping other/tap because it is not trusted\n"
	if err != nil || !strings.HasPrefix(out, want) {
		t.Errorf("out = %q, err = %v\nwant it to start with %q", out, err, want)
	}
}
