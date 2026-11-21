package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// publishTapCask commits a version of a cask to a tap repository, with the
// listing brew would generate for it.
func publishTapCask(t *testing.T, repo string, c map[string]any, version string) {
	t.Helper()
	token := c["token"].(string)
	rb := fmt.Sprintf("cask %q do\n  version %q\nend\n", token, version)
	sum := sha256.Sum256([]byte(rb))
	entry := map[string]any{}
	maps.Copy(entry, c)
	entry["tap"], entry["full_token"], entry["version"] = "jigsmiths/tap", "jigsmiths/tap/"+token, version
	entry["ruby_source_path"] = "Casks/" + token + ".rb"
	entry["ruby_source_checksum"] = map[string]string{"sha256": hex.EncodeToString(sum[:])}
	listing, err := json.Marshal([]any{entry})
	if err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string][]byte{"Casks/" + token + ".rb": []byte(rb), "api/cask.json": listing} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, path), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", token+" "+version)
}

// tapStore serves an app cask from a tap repository rather than the API.
func tapStore(t *testing.T) (e *env, prefix, origin string, app map[string]any) {
	t.Helper()
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
	if w := e.err.(*bytes.Buffer).String(); !strings.HasPrefix(w, "==> Tapping jigsmiths/tap\n") {
		t.Errorf("stderr = %q", w)
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

func TestInstallByFullNameFromATapWithoutAListing(t *testing.T) {
	e, prefix, origin, _ := tapStore(t)
	gitIn(t, origin, "rm", "-q", "api/cask.json")
	gitIn(t, origin, "commit", "-q", "-m", "no listing")
	stderr := &bytes.Buffer{}
	e.err = stderr
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "jigsmiths/tap/app"); err == nil {
		t.Error("installed a cask its tap does not list")
	}
	if !strings.Contains(stderr.String(), "api/cask.json") {
		t.Errorf("stderr should say the tap lists no casks for brw: %q", stderr)
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
