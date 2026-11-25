package cask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/platform"
)

type testInstall struct {
	in             *Installer
	prefix, appDir string
	out, warn      *bytes.Buffer
	url            string
	hits           *atomic.Int32
	body           []byte
}

func newTestInstall(t *testing.T, body []byte) *testInstall {
	t.Helper()
	prefix, appDir := t.TempDir(), t.TempDir()
	root, err := fsx.Open(prefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	apps, err := fsx.Open(appDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close(); _ = apps.Close() })
	srv, hits := server(t, body, nil)
	out, warn := &bytes.Buffer{}, &bytes.Buffer{}
	in := &Installer{
		Warn: warn,
		Root: root, Apps: apps,
		Where:   Where{Prefix: prefix, AppDir: appDir, Home: "/Users/me"},
		Host:    platform.Host{OS: "darwin", MacVersion: "26.1", Arch: "arm64"},
		Fetcher: &Fetcher{Dir: t.TempDir()},
		Log:     out,
		Now:     func() time.Time { return time.Date(2026, 11, 19, 10, 30, 45, 123e6, time.UTC) },
		Sleep:   func(time.Duration) {},
		Version: "0.9.0",
		APIPath: "/cache/api/internal/packages.arm64_tahoe.jws.json",
		Config:  Config{Languages: []string{"en-ZA"}, ExplicitAppDir: appDir},
	}
	return &testInstall{in: in, prefix: prefix, appDir: appDir, out: out, warn: warn, url: srv.URL + "/App-1.0.tar.gz", hits: hits, body: body}
}

func (ti *testInstall) cask(t *testing.T, artifacts string) api.Cask {
	t.Helper()
	return api.Cask{
		Token: "app", Tap: "homebrew/cask", TapGitHead: "abc123", Version: "1.0",
		URL: ti.url, SHA256: sum(ti.body), Artifacts: raws(t, artifacts),
	}
}

var appTarball = func() []byte {
	var t testing.T
	return tarGzBytes(&t,
		entry{name: "App.app/Contents/Info.plist", body: "<plist/>"},
		entry{name: "App.app/Contents/MacOS/app", body: "#!/bin/sh\n", mode: 0o755},
		entry{name: "tool", body: "#!/bin/sh\n"})
}()

const appArtifacts = `[
	{"uninstall":[{"quit":"com.example.app"}]},
	{"app":["App.app"],"target":"$APPDIR/App.app"},
	{"binary":["$APPDIR/App.app/Contents/MacOS/app"],"target":"$HOMEBREW_PREFIX/bin/app"},
	{"binary":["tool"]},
	{"zap":[{"trash":"~/Library/Preferences/com.example.app.plist"}]}]`

func readLink(t *testing.T, path string) string {
	t.Helper()
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestInstallAppAndBinaries(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	if err := ti.in.Install(context.Background(), ti.cask(t, appArtifacts)); err != nil {
		t.Fatal(err)
	}
	version := filepath.Join(ti.prefix, "Caskroom/app/1.0")
	app := filepath.Join(ti.appDir, "App.app")
	if _, err := os.Stat(filepath.Join(app, "Contents/Info.plist")); err != nil {
		t.Errorf("app not moved: %v", err)
	}
	if got := readLink(t, filepath.Join(version, "App.app")); got != app {
		t.Errorf("Caskroom link -> %s, want %s", got, app)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/app")); got != filepath.Join(app, "Contents/MacOS/app") {
		t.Errorf("bin/app -> %s", got)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/tool")); got != filepath.Join(version, "tool") {
		t.Errorf("bin/tool -> %s", got)
	}
	if fi, err := os.Stat(filepath.Join(version, "tool")); err != nil || fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("tool should be executable: %v %v", fi, err)
	}
	if fi, err := os.Stat(app); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("app mode = %v, %v; want a+rX,go-w", fi, err)
	}

	caskfile := filepath.Join(ti.prefix, "Caskroom/app/.metadata/1.0/20261119103045.123/Casks/app.json")
	if b, err := os.ReadFile(caskfile); err != nil || string(b) != "{}" {
		t.Errorf("caskfile = %q, %v", b, err)
	}
	config, err := os.ReadFile(filepath.Join(ti.prefix, "Caskroom/app/.metadata/config.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := `{"default":{"languages":["en-ZA"],"appdir":"/Applications","appimagedir":"/Users/me/Applications",` +
		`"keyboard_layoutdir":"/Library/Keyboard Layouts","colorpickerdir":"/Users/me/Library/ColorPickers",` +
		`"prefpanedir":"/Users/me/Library/PreferencePanes","qlplugindir":"/Users/me/Library/QuickLook",` +
		`"mdimporterdir":"/Users/me/Library/Spotlight","dictionarydir":"/Users/me/Library/Dictionaries",` +
		`"fontdir":"/Users/me/Library/Fonts","servicedir":"/Users/me/Library/Services",` +
		`"input_methoddir":"/Users/me/Library/Input Methods","internet_plugindir":"/Users/me/Library/Internet Plug-Ins",` +
		`"audio_unit_plugindir":"/Users/me/Library/Audio/Plug-Ins/Components","vst_plugindir":"/Users/me/Library/Audio/Plug-Ins/VST",` +
		`"vst3_plugindir":"/Users/me/Library/Audio/Plug-Ins/VST3","screen_saverdir":"/Users/me/Library/Screen Savers"},` +
		`"env":{},"explicit":{"appdir":"` + ti.appDir + `"}}`
	if string(config) != wantConfig {
		t.Errorf("config.json =\n%s\nwant\n%s", config, wantConfig)
	}
	if got := caskroom.AppDir(ti.prefix, "app"); got != ti.appDir {
		t.Errorf("caskroom.AppDir = %s", got)
	}

	receipt, err := os.ReadFile(filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil {
		t.Fatal(err)
	}
	wantReceipt := `{
  "homebrew_version": "0.9.0",
  "loaded_from_api": true,
  "loaded_from_internal_api": true,
  "uninstall_flight_blocks": false,
  "installed_on_request": true,
  "time": 1795084245,
  "runtime_dependencies": {},
  "source": {
    "tap": "homebrew/cask",
    "tap_git_head": "abc123",
    "version": "1.0",
    "path": "/cache/api/internal/packages.arm64_tahoe.jws.json"
  },
  "arch": "arm64",
  "uninstall_artifacts": [
    {
      "uninstall": [
        {
          "quit": "com.example.app"
        }
      ]
    },
    {
      "app": [
        "App.app"
      ]
    },
    {
      "binary": [
        "` + app + `/Contents/MacOS/app"
      ]
    },
    {
      "binary": [
        "tool"
      ]
    },
    {
      "zap": [
        {
          "trash": "~/Library/Preferences/com.example.app.plist"
        }
      ]
    }
  ],
  "built_on": {
    "os": "Macintosh",
    "os_version": "macOS 26.1"
  }
}`
	if string(receipt) != wantReceipt {
		t.Errorf("receipt =\n%s\nwant\n%s", receipt, wantReceipt)
	}
	cs, err := caskroom.Installed(ti.prefix)
	if err != nil || len(cs) != 1 || cs[0].Token != "app" || cs[0].Version != "1.0" || !cs[0].OnRequest || cs[0].Tap != "homebrew/cask" {
		t.Errorf("caskroom.Installed = %+v, %v", cs, err)
	}

	wantOut := "==> Installing Cask app\n" +
		"==> Moving App 'App.app' to '" + app + "'\n" +
		"==> Linking Binary 'app' to '" + filepath.Join(ti.prefix, "bin/app") + "'\n" +
		"==> Linking Binary 'tool' to '" + filepath.Join(ti.prefix, "bin/tool") + "'\n" +
		"🍺  app was successfully installed!\n"
	if got := ti.out.String(); got != wantOut {
		t.Errorf("output =\n%s\nwant\n%s", got, wantOut)
	}
}

func TestInstallReceiptOfATapCask(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	c := ti.cask(t, `[{"binary":["tool"]}]`)
	c.Tap, c.SourceFile = "jigsmiths/tap", "/opt/homebrew/Library/Taps/jigsmiths/homebrew-tap/Casks/app.rb"
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		LoadedFromAPI         bool `json:"loaded_from_api"`
		LoadedFromInternalAPI bool `json:"loaded_from_internal_api"`
		Source                struct{ Tap, Path string }
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if r.LoadedFromAPI || r.LoadedFromInternalAPI || r.Source.Tap != c.Tap || r.Source.Path != c.SourceFile {
		t.Errorf("receipt = %s", b)
	}
}

const flightSteps = `[{"binary":["tool"]},
	{"postflight_steps":[{"steps":[
		{"type":"run","command":{"path":"/usr/bin/touch"},"args":["{{staged_path}}/post-{{token}}"]},
		{"type":"symlink","source":{"base":"staged_path","path":"tool"},"target":{"base":"homebrew_prefix","path":"bin/tool-alias"},"uninstall":true}]}]},
	{"preflight_steps":[{"steps":[{"type":"touch","path":{"base":"staged_path","path":"pre"}}]}]}]`

func TestInstallRunsFlightStepsAndUninstallUndoesTheirLinks(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	if err := ti.in.Install(context.Background(), ti.cask(t, flightSteps)); err != nil {
		t.Fatal(err)
	}
	version := filepath.Join(ti.prefix, "Caskroom/app/1.0")
	for _, f := range []string{"pre", "post-app"} {
		if _, err := os.Stat(filepath.Join(version, f)); err != nil {
			t.Errorf("a step did not make %s: %v", f, err)
		}
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/tool-alias")); got != filepath.Join(version, "tool") {
		t.Errorf("bin/tool-alias -> %s", got)
	}
	receipt, err := os.ReadFile(filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil || !strings.Contains(string(receipt), `"postflight_steps"`) || !strings.Contains(string(receipt), `"{{staged_path}}/post-{{token}}"`) {
		t.Errorf("the receipt should keep the steps as listed: %s %v", receipt, err)
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(ti.prefix, "bin/tool-alias")); err == nil {
		t.Error("uninstall left the step's link")
	}
	if _, err := os.Stat(version); err == nil {
		t.Error("uninstall left the version")
	}
}

func TestInstallRefusesStepsBrwCannotRun(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	c := ti.cask(t, `[{"binary":["tool"]},{"postflight_steps":[{"steps":[{"type":"terminate_process","name":"x"}]}]}]`)
	if err := ti.in.Install(context.Background(), c); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "terminate_process") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(ti.prefix, "Caskroom/app")); err == nil {
		t.Error("a refused cask was installed")
	}
}

func TestInstallLeavesNoTemporaryFiles(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	if err := ti.in.Install(context.Background(), ti.cask(t, appArtifacts)); err != nil {
		t.Fatal(err)
	}
	wantTree(t, filepath.Join(ti.prefix, "Caskroom/app"),
		".metadata/", ".metadata/1.0/", ".metadata/1.0/20261119103045.123/", ".metadata/1.0/20261119103045.123/Casks/",
		".metadata/1.0/20261119103045.123/Casks/app.json", ".metadata/INSTALL_RECEIPT.json", ".metadata/config.json",
		"1.0/", "1.0/App.app", "1.0/tool")
}

func TestInstallRefusesAnExistingApp(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	if err := os.MkdirAll(filepath.Join(ti.appDir, "App.app"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := ti.in.Install(context.Background(), ti.cask(t, appArtifacts))
	want := "app: It seems there is already an App at '" + filepath.Join(ti.appDir, "App.app") + "'."
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
	if ti.hits.Load() != 0 {
		t.Error("downloaded before checking the targets")
	}
	if _, err := os.Stat(filepath.Join(ti.prefix, "Caskroom/app")); !os.IsNotExist(err) {
		t.Errorf("Caskroom/app left behind: %v", err)
	}
}

func TestInstallRefusesAnExistingBinary(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	writeFile(t, filepath.Join(ti.prefix, "bin/tool"), []byte("mine"), 0o755)
	err := ti.in.Install(context.Background(), ti.cask(t, appArtifacts))
	if want := "app: It seems there is already a Binary at '" + filepath.Join(ti.prefix, "bin/tool") + "'."; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func TestInstallSkipsAFormulasBinary(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	writeFile(t, filepath.Join(ti.prefix, "Cellar/tool/2.0/bin/tool"), []byte("x"), 0o755)
	if err := os.Mkdir(filepath.Join(ti.prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/tool/2.0/bin/tool", filepath.Join(ti.prefix, "bin/tool")); err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Install(context.Background(), ti.cask(t, appArtifacts)); err != nil {
		t.Fatal(err)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/tool")); got != "../Cellar/tool/2.0/bin/tool" {
		t.Errorf("formula link replaced with %s", got)
	}
	if want := "Warning: It seems there is already a Binary at '" + filepath.Join(ti.prefix, "bin/tool") + "' from formula tool; skipping link.\n"; ti.warn.String() != want {
		t.Errorf("warnings = %q, want %q", ti.warn, want)
	}
}

func TestInstallUndoesAFailedInstall(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	c := ti.cask(t, `[{"app":["App.app"]},{"binary":["tool"]},{"binary":["missing"]}]`)
	err := ti.in.Install(context.Background(), c)
	if want := "app: It seems the symlink source '" + filepath.Join(ti.prefix, "Caskroom/app/1.0/missing") + "' is not there."; err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %s", err, want)
	}
	for _, p := range []string{filepath.Join(ti.appDir, "App.app"), filepath.Join(ti.prefix, "bin/tool"), filepath.Join(ti.prefix, "Caskroom/app")} {
		if _, err := os.Lstat(p); !os.IsNotExist(err) {
			t.Errorf("%s left behind: %v", p, err)
		}
	}
	if !strings.Contains(ti.out.String(), "==> Purging files for version 1.0 of Cask app\n") {
		t.Errorf("output:\n%s", ti.out)
	}
}

func TestInstallChecksTheCaskFirst(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	c := ti.cask(t, appArtifacts)
	c.DependsOn.MacOS = &api.MacOSRequirement{Comparator: ">=", Versions: []string{"27"}}
	if err := ti.in.Install(context.Background(), c); err == nil || !strings.Contains(err.Error(), "older than Golden Gate") {
		t.Errorf("err = %v", err)
	}
	c = ti.cask(t, `[{"pkg":["a.pkg"]}]`)
	if _, err := json.Marshal(c); err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Install(context.Background(), c); err == nil {
		t.Error("installed a pkg cask")
	}
	if ti.hits.Load() != 0 {
		t.Error("downloaded a cask that cannot be installed")
	}
}

func TestInstallRefusesADisabledCask(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	c := ti.cask(t, appArtifacts)
	c.Disabled, c.DisableReason, c.DisableDate = true, "fails_gatekeeper_check", "2026-09-01"
	err := ti.in.Install(context.Background(), c)
	want := "Cask 'app' has been disabled because it does not pass the macOS Gatekeeper check! It was disabled on 2026-09-01."
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
	if ti.hits.Load() != 0 {
		t.Error("downloaded a disabled cask")
	}
}

func TestInstallWarnsAboutADeprecatedCask(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	warn := &bytes.Buffer{}
	ti.in.Warn = warn
	c := ti.cask(t, appArtifacts)
	c.Deprecated, c.DeprecationReason, c.DeprecationDate = true, "discontinued", "2025-10-01"
	c.DeprecationReplacementCask = "other"
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := "Warning: app has been deprecated because it is discontinued upstream! It was disabled on 2026-10-01.\n" +
		"Replacement:\n  brw install --cask other\n\n"
	if warn.String() != want {
		t.Errorf("warning = %q, want %q", warn, want)
	}
}

func TestDeprecationMessage(t *testing.T) {
	now := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		c    api.Cask
		want string
	}{
		{api.Cask{}, ""},
		{api.Cask{Deprecated: true}, "deprecated!"},
		{api.Cask{Deprecated: true, DeprecationReason: "a custom reason", DeprecationDate: "2026-01-31"},
			"deprecated because it a custom reason! It will be disabled on 2027-01-31."},
		{api.Cask{Disabled: true, DisableReason: "unmaintained", DisableDate: "2026-12-01", DisableReplacementFormula: "x", DisableReplacementCask: "x"},
			"disabled because it is not maintained upstream! It will be disabled on 2026-12-01.\nReplacement:\n  brw install x\n"},
	} {
		if got := deprecation(tc.c, now); got != tc.want {
			t.Errorf("%+v: got %q, want %q", tc.c, got, tc.want)
		}
	}
}

func TestInstallLinksABinaryNamedByItsPathInThePrefix(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	c := ti.cask(t, `[{"binary":["$HOMEBREW_PREFIX/Caskroom/app/1.0/tool"]}]`)
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/tool")); got != filepath.Join(ti.prefix, "Caskroom/app/1.0/tool") {
		t.Errorf("bin/tool -> %s", got)
	}
}

func TestInstallRefusesTargetsOutsideItsFolders(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	c := ti.cask(t, `[{"app":["App.app",{"target":"/Users/Shared/App.app"}]}]`)
	if err := ti.in.Install(context.Background(), c); err == nil || !regexp.MustCompile(`outside`).MatchString(err.Error()) {
		t.Errorf("err = %v", err)
	}
}

func TestInstallIntoALockedAppFolder(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}}
	ti.in.Exec = f.exec
	lock(t, ti.appDir)
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"app":["App.app"]}]`)); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(ti.appDir, "App.app")
	if !exists(filepath.Join(app, "Contents/Info.plist")) {
		t.Error("the app should be in the app folder")
	}
	if got := readLink(t, filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")); got != app {
		t.Errorf("Caskroom link -> %s", got)
	}
	staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")
	if got := f.ran("/usr/bin/sudo"); !slices.Equal(got, []string{"/usr/bin/sudo -E -- /bin/cp -pR " + staged + " " + app}) {
		t.Errorf("sudo calls = %q", got)
	}
}
