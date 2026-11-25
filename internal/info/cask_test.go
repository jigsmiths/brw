package info

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/outdated"
)

func casks(t *testing.T) *api.CaskIndex {
	t.Helper()
	b, err := os.ReadFile("../api/testdata/casks.json")
	if err != nil {
		t.Fatal(err)
	}
	cs, err := api.ParseCasks(b)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewCaskIndex(cs)
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// installCask records an install the way brew does, with a payload of size bytes.
func installCask(t *testing.T, prefix, token, version string, onRequest bool, size int) {
	t.Helper()
	room := filepath.Join(prefix, "Caskroom", token)
	writeTestFile(t, filepath.Join(room, ".metadata", version, "20261117083153.956", "Casks", token+".json"), "{}")
	receipt, _ := json.Marshal(map[string]any{"installed_on_request": onRequest, "source": map[string]string{"tap": "homebrew/cask"}})
	writeTestFile(t, filepath.Join(room, ".metadata", "INSTALL_RECEIPT.json"), string(receipt))
	writeTestFile(t, filepath.Join(room, version, "payload"), strings.Repeat("x", size))
}

func caskEnv(t *testing.T, prefix string) CaskEnv {
	t.Helper()
	installed, err := caskroom.Installed(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return CaskEnv{
		Casks:     casks(t),
		Host:      mac,
		Prefix:    prefix,
		Installed: installed,
		Outdated: outdated.CaskOptions{
			AppDir: func(string) string { return "/Applications" },
			Bundle: func(string) (string, string, bool) { return "", "", false },
		},
		Home:    "/Users/me",
		Rosetta: true,
		Now:     time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC),
	}
}

func caskText(t *testing.T, env CaskEnv, token string) string {
	t.Helper()
	c, ok := env.Casks.Lookup(token)
	if !ok {
		t.Fatalf("no cask %s", token)
	}
	text, err := CaskText(c, env)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

// A disabled cask reads as disabled even while still deprecated, and a Linux
// requirement is listed, as brew does.
func TestCaskTextDisabledAndLinux(t *testing.T) {
	var c api.Cask
	if err := json.Unmarshal([]byte(`{"token":"old","full_token":"old","tap":"homebrew/cask","name":["Old"],"version":"1.0",
		"deprecated":true,"deprecation_reason":"discontinued","disabled":true,"disable_reason":"discontinued","disable_date":"2026-03-16",
		"depends_on":{"arch":[{"type":"intel","bits":64}],"linux":"#<LinuxRequirement:0x1>"},"artifacts":[]}`), &c); err != nil {
		t.Fatal(err)
	}
	env := caskEnv(t, t.TempDir())
	env.Casks = api.NewCaskIndex([]api.Cask{c})
	got := caskText(t, env, "old")
	for _, want := range []string{
		"\nDisabled because it is discontinued upstream! It was disabled on 2026-03-16.\n",
		"==> Requirements\nRequired: x86_64 architecture, Linux\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("want %q in\n%s", want, got)
		}
	}
}

func TestCaskTextInstalled(t *testing.T) {
	prefix := t.TempDir()
	installCask(t, prefix, "slack", "4.53.292", true, 1500)
	want := `==> slack (Slack): 4.53.292 (auto updates)
Team communication and collaboration software
https://slack.com/
Installed (on request)
From: https://github.com/Homebrew/homebrew-cask/blob/HEAD/Casks/s/slack.rb
==> Installed Versions
slack 4.53.292 (1.5KB)
==> Requirements
Required: macOS
==> Apps
Slack.app
`
	if got := caskText(t, caskEnv(t, prefix), "slack"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestCaskTextOutdated(t *testing.T) {
	prefix := t.TempDir()
	installCask(t, prefix, "1password-cli", "2.40.0", false, 10)
	got := caskText(t, caskEnv(t, prefix), "1password-cli")
	for _, want := range []string{
		"==> 1password-cli (1Password CLI): 2.40.0 → 2.42.0\n",
		"Installed (as dependency)\n",
		"==> Installed Versions\n1password-cli 2.40.0 → 2.42.0 (10B)\n",
		"==> Artifacts\nop completion (base_name: op, shells: bash, zsh, fish) (Generated Completion)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if strings.Contains(got, "Requirements") {
		t.Errorf("1password-cli names no requirements:\n%s", got)
	}
}

func TestCaskTextRelatedInstalls(t *testing.T) {
	prefix := t.TempDir()
	installCask(t, prefix, "1password", "8.12.46", true, 2000)
	installCask(t, prefix, "1password-cli", "2.42.0", true, 10) // not a relative
	got := caskText(t, caskEnv(t, prefix), "1password@nightly")
	for _, want := range []string{
		"==> 1password@nightly (1Password Nightly): latest\n",
		"Not installed\n",
		"==> Installed Versions\n1password 8.12.46 (2KB)\n==> Requirements\nRequired: macOS >= 12\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestCaskTextMissingVersionDirectory(t *testing.T) {
	prefix := t.TempDir()
	installCask(t, prefix, "slack", "4.53.292", true, 1)
	if err := os.RemoveAll(filepath.Join(prefix, "Caskroom/slack/4.53.292")); err != nil {
		t.Fatal(err)
	}
	want := "==> Installed Versions\nslack 4.53.292\n  " + filepath.Join(prefix, "Caskroom/slack/4.53.292") + " (does not exist)\n"
	if got := caskText(t, caskEnv(t, prefix), "slack"); !strings.Contains(got, want) {
		t.Errorf("missing %q in\n%s", want, got)
	}
}

func TestCaskTextDependencies(t *testing.T) {
	prefix := t.TempDir()
	env := caskEnv(t, prefix)
	want := "==> Dependencies\nRequired (1): libfido2\nRecursive Runtime (1): 0 installed ✔, 1 missing ✘\n"
	if got := caskText(t, env, "aptible"); !strings.Contains(got, want) {
		t.Errorf("missing %q in\n%s", want, got)
	}

	// An installed formula brings the runtime dependencies its keg recorded.
	receipt, err := cellar.ParseReceipt([]byte(`{"runtime_dependencies": [{"full_name": "openssl@3"}, {"full_name": "zlib"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	env.Kegs = []cellar.Keg{{Name: "libfido2", Version: "1.16.0", Receipt: receipt}, {Name: "zlib", Version: "1.3"}}
	want = "Required (1): libfido2\nRecursive Runtime (3): 2 installed ✔, 1 missing ✘\n"
	if got := caskText(t, env, "aptible"); !strings.Contains(got, want) {
		t.Errorf("missing %q in\n%s", want, got)
	}
}

func TestCaskTextCaveats(t *testing.T) {
	prefix := t.TempDir()
	env := caskEnv(t, prefix)
	got := caskText(t, env, "gcloud-cli")
	want := `==> Caveats
To use additional binary components installed via gcloud, add the "` + prefix + `/share/google-cloud-sdk/bin"
directory to your PATH environment variable, e.g., (for Bash shell):
   export PATH=` + prefix + `/share/google-cloud-sdk/bin:"$PATH"

`
	if !strings.HasSuffix(got, want) {
		t.Errorf("caveats:\n%s\nwant suffix\n%s", got, want)
	}
	for _, want := range []string{
		"==> Artifacts\ngoogle-cloud-sdk/install.sh (Installer)\n",
		"google-cloud-sdk/completion.bash.inc -> google-cloud-sdk (Bash Completion)\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestCaskTextDeprecatedNeedsRosetta(t *testing.T) {
	env := caskEnv(t, t.TempDir())
	env.Rosetta = false
	want := `==> bridge (Quixel Bridge): 2025.0.0 (auto updates)
3D asset manager
https://quixel.com/
Deprecated because it is discontinued upstream! It will be disabled on 2026-12-18.
Replacement:
  brw install --cask epic-games
Not installed
From: https://github.com/Homebrew/homebrew-cask/blob/HEAD/Casks/b/bridge.rb
==> Requirements
Required: macOS
==> Apps
Bridge.app
==> Caveats
bridge is built for Intel macOS and so requires Rosetta 2 to be installed.
You can install Rosetta 2 with:
  softwareupdate --install-rosetta --agree-to-license
Note that it is very difficult to remove Rosetta 2 once it is installed.

`
	if got := caskText(t, env, "bridge"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	env.Rosetta = true
	if got := caskText(t, env, "bridge"); strings.Contains(got, "Caveats") {
		t.Errorf("Rosetta 2 is installed, so no caveat:\n%s", got)
	}
}

func TestCaskTextFromAnotherTap(t *testing.T) {
	prefix := t.TempDir()
	installCask(t, prefix, "slack", "4.53.292", true, 1)
	env := caskEnv(t, prefix)
	env.Installed[0].Tap = "other/tap"
	c, _ := env.Casks.Lookup("slack")
	if _, err := CaskText(c, env); !errors.Is(err, ErrOtherTap) {
		t.Errorf("err = %v, want ErrOtherTap", err)
	}
}

func TestCaskRequirements(t *testing.T) {
	c := api.Cask{}
	c.DependsOn.Arch = append(c.DependsOn.Arch, struct {
		Type string `json:"type"`
		Bits int    `json:"bits"`
	}{"arm", 64}, struct {
		Type string `json:"type"`
		Bits int    `json:"bits"`
	}{"intel", 64})
	c.DependsOn.MacOS = &api.MacOSRequirement{Comparator: "==", Versions: []string{"14", "15"}}
	if got := caskRequirements(c); got != "==> Requirements\nRequired: arm64 architecture, x86_64 architecture, macOS == 14 / 15\n" {
		t.Errorf("got %q", got)
	}
	if got := caskRequirements(api.Cask{}); got != "" {
		t.Errorf("no requirements gave %q", got)
	}
}

func TestArtifactSections(t *testing.T) {
	c := api.Cask{Token: "x"}
	for _, a := range []string{
		`{"zap": [{"trash": "~/x"}]}`,
		`{"binary": ["x"], "target": "/opt/homebrew/bin/x"}`,
		`{"manpage": ["x.1"], "target": "/opt/homebrew/share/man/man1/x.1"}`,
		`{"qlplugin": ["X.qlgenerator"], "target": "/Users/me/Library/QuickLook/X.qlgenerator"}`,
		`{"pkg": ["X.pkg", {"allow_untrusted": true}]}`,
		`{"font": ["x.ttf"], "target": "/Users/me/Library/Fonts/x.ttf"}`,
		`{"app": ["X.app", {"target": "Y.app"}], "target": "/Applications/Y.app"}`,
		`{"installer": [{"manual": "Install X.app"}]}`,
		`{"zsh_completion": ["$APPDIR/X.app/x.zsh"], "target": "/opt/homebrew/share/zsh/site-functions/_x"}`,
	} {
		c.Artifacts = append(c.Artifacts, json.RawMessage(a))
	}
	want := `==> Apps
X.app -> Y.app
==> Fonts
x.ttf
==> Manpages
x.1
==> Plugins
X.qlgenerator (Quick Look Plugin)
==> Artifacts
Install X.app (Installer)
X.pkg (Pkg)
/Apps/X.app/x.zsh (Zsh Completion)`
	fill := strings.NewReplacer("$APPDIR", "/Apps").Replace
	if got := artifactSections(c, fill); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestCaskDeprecation(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		cask api.Cask
		want string
	}{
		{api.Cask{}, ""},
		{api.Cask{Deprecated: true}, "Deprecated!"},
		{api.Cask{Deprecated: true, DeprecationReason: "moved_to_mas", DeprecationDate: "2026-01-01"},
			"Deprecated because it is now exclusively distributed on the Mac App Store! It will be disabled on 2027-01-01."},
		// An estimate that has passed is left out.
		{api.Cask{Deprecated: true, DeprecationReason: "some reason", DeprecationDate: "2025-01-01"},
			"Deprecated because it some reason!"},
		{api.Cask{Disabled: true, DisableReason: "unreachable", DisableDate: "2026-02-01", DisableReplacementFormula: "x"},
			"Disabled because it is no longer reliably reachable upstream! It was disabled on 2026-02-01.\nReplacement:\n  brw install --formula x\n"},
	} {
		if got := CaskDeprecation(&tc.cask, now); got != tc.want {
			t.Errorf("CaskDeprecation(%+v) = %q, want %q", tc.cask, got, tc.want)
		}
	}
}
