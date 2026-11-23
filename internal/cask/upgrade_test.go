package cask

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
)

func (in *Installer) Upgrade(ctx context.Context, old caskroom.Cask, c api.Cask) error {
	u, err := in.PrepareUpgrade(ctx, old, c)
	if err != nil {
		return err
	}
	return u.Run(ctx)
}

var appTarball2 = func() []byte {
	var t testing.T
	return tarGzBytes(&t,
		entry{name: "App.app/Contents/Info.plist", body: "<plist>2</plist>"},
		entry{name: "App.app/Contents/MacOS/app", body: "#!/bin/sh\n", mode: 0o755},
		entry{name: "tool", body: "#!/bin/sh\n"})
}()

// upgradable installs app 1.0 and returns the 2.0 that replaces it.
func upgradable(t *testing.T, artifacts string) (*testInstall, *fakeExec, api.Cask) {
	t.Helper()
	ti, f := installed(t, artifacts)
	srv, _ := server(t, appTarball2, nil)
	return ti, f, api.Cask{
		Token: "app", Tap: "homebrew/cask", TapGitHead: "abc123", Version: "2.0",
		URL: srv.URL + "/App-2.0.tar.gz", SHA256: sum(appTarball2), Artifacts: raws(t, artifacts),
	}
}

func installedCask(t *testing.T, prefix string) caskroom.Cask {
	t.Helper()
	cs, err := caskroom.Installed(prefix)
	if err != nil || len(cs) != 1 {
		t.Fatalf("caskroom.Installed = %+v, %v", cs, err)
	}
	return cs[0]
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestUpgradeAppAndBinaries(t *testing.T) {
	ti, _, c := upgradable(t, appArtifacts)
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(ti.appDir, "App.app")
	want := strings.Join([]string{
		"==> Upgrading app",
		"  1.0 -> 2.0",
		"==> Backing up App 'App.app' to '" + filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app") + "'",
		"==> Removing App '" + app + "'",
		"==> Unlinking Binary '" + filepath.Join(ti.prefix, "bin/app") + "'",
		"==> Unlinking Binary '" + filepath.Join(ti.prefix, "bin/tool") + "'",
		"==> Moving App 'App.app' to '" + app + "'",
		"==> Linking Binary 'app' to '" + filepath.Join(ti.prefix, "bin/app") + "'",
		"==> Linking Binary 'tool' to '" + filepath.Join(ti.prefix, "bin/tool") + "'",
		"==> Purging files for version 1.0 of Cask app",
		"🍺  app was successfully upgraded!",
	}, "\n") + "\n"
	if got := ti.out.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	if got := readFile(t, filepath.Join(app, "Contents/Info.plist")); got != "<plist>2</plist>" {
		t.Errorf("the app is not the new one: Info.plist = %q", got)
	}
	version := filepath.Join(ti.prefix, "Caskroom/app/2.0")
	if got := readLink(t, filepath.Join(version, "App.app")); got != app {
		t.Errorf("Caskroom link -> %s, want %s", got, app)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/app")); got != filepath.Join(app, "Contents/MacOS/app") {
		t.Errorf("bin/app -> %s", got)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/tool")); got != filepath.Join(version, "tool") {
		t.Errorf("bin/tool -> %s", got)
	}
	wantTree(t, filepath.Join(ti.prefix, "Caskroom/app"),
		".metadata/", ".metadata/2.0/", ".metadata/2.0/20261119103045.123/", ".metadata/2.0/20261119103045.123/Casks/",
		".metadata/2.0/20261119103045.123/Casks/app.json", ".metadata/INSTALL_RECEIPT.json", ".metadata/config.json",
		"2.0/", "2.0/App.app", "2.0/tool")
	if got := installedCask(t, ti.prefix); got.Version != "2.0" || !got.OnRequest {
		t.Errorf("installed = %+v", got)
	}
	if !strings.Contains(readFile(t, filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")), `"version": "2.0"`) {
		t.Error("the receipt does not name 2.0")
	}
}

func TestUpgradeKeepsWhetherTheCaskWasRequested(t *testing.T) {
	ti, _, c := upgradable(t, appArtifacts)
	receipt := filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")
	s := strings.Replace(readFile(t, receipt), `"installed_on_request": true`, `"installed_on_request": false`, 1)
	writeFile(t, receipt, []byte(s), 0o644)
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err != nil {
		t.Fatal(err)
	}
	if got := installedCask(t, ti.prefix); got.Version != "2.0" || got.OnRequest {
		t.Errorf("installed = %+v, want 2.0 not on request", got)
	}
}

func TestUpgradeRefusesBeforeChangingAnything(t *testing.T) {
	ti, _, c := upgradable(t, appArtifacts)
	c.Artifacts = raws(t, `[{"pkg":["App.pkg"]}]`)
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
	if ti.out.Len() != 0 {
		t.Errorf("output:\n%s", ti.out)
	}
	if got := readFile(t, filepath.Join(ti.appDir, "App.app/Contents/Info.plist")); got != "<plist/>" {
		t.Errorf("Info.plist = %q", got)
	}
	if got := installedCask(t, ti.prefix); got.Version != "1.0" {
		t.Errorf("installed = %+v", got)
	}
}

func TestUpgradeRevertsAFailedUpgrade(t *testing.T) {
	ti, f, c := upgradable(t, appArtifacts)
	f.running["com.example.app"] = 1
	c.Artifacts = raws(t, `[{"uninstall":[{"quit":"com.example.app"}]},{"app":["Missing.app"]},{"binary":["tool"]}]`)
	err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c)
	if err == nil || !strings.Contains(err.Error(), "It seems the App source") {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(ti.warn.String(), "Warning: Reverting upgrade for Cask app\n") {
		t.Errorf("warnings:\n%s", ti.warn)
	}
	app := filepath.Join(ti.appDir, "App.app")
	if got := readFile(t, filepath.Join(app, "Contents/Info.plist")); got != "<plist/>" {
		t.Errorf("the old app is not back: Info.plist = %q", got)
	}
	version := filepath.Join(ti.prefix, "Caskroom/app/1.0")
	if got := readLink(t, filepath.Join(version, "App.app")); got != app {
		t.Errorf("Caskroom link -> %s", got)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/app")); got != filepath.Join(app, "Contents/MacOS/app") {
		t.Errorf("bin/app -> %s", got)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "bin/tool")); got != filepath.Join(version, "tool") {
		t.Errorf("bin/tool -> %s", got)
	}
	wantTree(t, filepath.Join(ti.prefix, "Caskroom/app"),
		".metadata/", ".metadata/1.0/", ".metadata/1.0/20261119103045.123/", ".metadata/1.0/20261119103045.123/Casks/",
		".metadata/1.0/20261119103045.123/Casks/app.json", ".metadata/INSTALL_RECEIPT.json", ".metadata/config.json",
		"1.0/", "1.0/App.app", "1.0/tool")
	if got := installedCask(t, ti.prefix); got.Version != "1.0" {
		t.Errorf("installed = %+v", got)
	}
	if strings.Contains(ti.out.String(), "Reopening") || len(f.ran("/usr/bin/env")) != 0 {
		t.Errorf("reopened after a failed upgrade:\n%s", ti.out)
	}
}

func TestUpgradeReopensTheAppsItQuit(t *testing.T) {
	ti, f, c := upgradable(t, appArtifacts)
	f.running["com.example.app"] = 1
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(ti.appDir, "App.app")
	wantEnd := "🍺  app was successfully upgraded!\n" +
		"==> Reopening 1 application closed during upgrade:\n" +
		"com.example.app\n"
	if got := ti.out.String(); !strings.HasSuffix(got, wantEnd) || !strings.Contains(got, "==> Quitting application 'com.example.app'...\n") {
		t.Errorf("output:\n%s\nwant it to end with:\n%s", got, wantEnd)
	}
	if got := f.ran("/usr/bin/env"); !slices.Equal(got, []string{"/usr/bin/env -i /usr/bin/open -b com.example.app"}) {
		t.Errorf("open calls = %q", got)
	}
	var wantRegister []string
	if exists(lsregister) {
		wantRegister = []string{lsregister + " -f " + app}
	}
	if got := f.ran(lsregister); !slices.Equal(got, wantRegister) {
		t.Errorf("lsregister calls = %q, want %q", got, wantRegister)
	}
}

func TestUpgradeReopensNothingWhenNothingWasRunning(t *testing.T) {
	ti, f, c := upgradable(t, appArtifacts)
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ti.out.String(), "Reopening") || len(f.ran("/usr/bin/env")) != 0 || len(f.ran(lsregister)) != 0 {
		t.Errorf("reopened an app that was not running:\n%s", ti.out)
	}
}

// exitCode is an error from a command that exited with the code.
type exitCode int

func (e exitCode) Error() string { return "exit status " + strconv.Itoa(int(e)) }
func (e exitCode) ExitCode() int { return int(e) }

func setQuarantine(t *testing.T, path, value string) {
	t.Helper()
	args := []string{"-w", quarantineAttr, value, path}
	if value == "" {
		args = []string{"-d", quarantineAttr, path}
	}
	if out, err := exec.Command(xattrTool, args...).CombinedOutput(); err != nil && value != "" {
		t.Fatalf("xattr %v: %v: %s", args, err, out)
	}
}

func TestUpgradeQuarantineApproval(t *testing.T) {
	if _, err := os.Stat(xattrTool); err != nil {
		t.Skip("needs xattr")
	}
	const designated = "designated => identifier \"com.example.app\" and anchor apple generic\n"
	tests := []struct {
		name       string
		quarantine string  // on the old app; "" for none
		identity   error   // from codesign -d
		verify     []error // from each codesign --verify: the old app's, then the new one's
		wantWarn   string
		wantFlags  string // the new app's quarantine flags
	}{
		{name: "approved and same signer", quarantine: "01c1;5f000000;;OLD", verify: []error{nil, nil}, wantFlags: "01c1"},
		{name: "not quarantined counts as approved", verify: []error{nil, nil}, wantFlags: "01c1"},
		{name: "never approved", quarantine: "0181;5f000000;;OLD", verify: []error{nil}, wantFlags: "0181"},
		{
			name: "signer changed", quarantine: "01c1;5f000000;;OLD", verify: []error{nil, exitCode(3)},
			wantWarn: "Warning: app's signer changed so macOS may prompt at next launch.\n", wantFlags: "0181",
		},
		{
			name: "new signature unreadable", quarantine: "01c1;5f000000;;OLD", verify: []error{nil, exitCode(1)},
			wantWarn: "Warning: Homebrew couldn't verify app's signer so macOS may prompt at next launch.\n", wantFlags: "0181",
		},
		{
			name: "old app unsigned", quarantine: "01c1;5f000000;;OLD", identity: exitCode(1),
			wantWarn: "Warning: Homebrew couldn't verify app's signer so macOS may prompt at next launch.\n", wantFlags: "0181",
		},
		{
			name: "old app fails its own requirement", quarantine: "01c1;5f000000;;OLD", verify: []error{exitCode(3)},
			wantWarn: "Warning: Homebrew couldn't verify app's signer so macOS may prompt at next launch.\n", wantFlags: "0181",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ti, f, c := upgradable(t, appArtifacts)
			app := filepath.Join(ti.appDir, "App.app")
			setQuarantine(t, app, tt.quarantine)
			verifies := 0
			f.handle = map[string]func([]string) ([]byte, error){
				"/usr/bin/codesign": func(args []string) ([]byte, error) {
					if args[0] == "-d" {
						if tt.identity != nil {
							return nil, tt.identity
						}
						return []byte(designated), nil
					}
					verifies++
					if verifies > len(tt.verify) {
						t.Errorf("unexpected codesign %q", args)
						return nil, exitCode(1)
					}
					return nil, tt.verify[verifies-1]
				},
			}
			if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err != nil {
				t.Fatal(err)
			}
			if got := ti.warn.String(); got != tt.wantWarn {
				t.Errorf("warnings = %q, want %q", got, tt.wantWarn)
			}
			for _, p := range []string{app, filepath.Join(app, "Contents/Info.plist")} {
				if got, _, _ := strings.Cut(readQuarantine(context.Background(), p), ";"); got != tt.wantFlags {
					t.Errorf("%s quarantine flags = %q, want %q", p, got, tt.wantFlags)
				}
			}
			if verifies != len(tt.verify) {
				t.Errorf("codesign --verify ran %d times, want %d", verifies, len(tt.verify))
			}
			for _, call := range f.ran("/usr/bin/codesign --verify") {
				want := `/usr/bin/codesign --verify --deep --strict --all-architectures -R =identifier "com.example.app" and anchor apple generic ` + app
				if call != want {
					t.Errorf("codesign call = %q, want %q", call, want)
				}
			}
		})
	}
}

func TestInheritApprovalSkipsSymlinks(t *testing.T) {
	if _, err := os.Stat(xattrTool); err != nil {
		t.Skip("needs xattr")
	}
	app := filepath.Join(t.TempDir(), "App.app")
	writeFile(t, filepath.Join(app, "Contents/Info.plist"), []byte("<plist/>"), 0o644)
	outside := filepath.Join(t.TempDir(), "outside")
	writeFile(t, outside, []byte("x"), 0o644)
	if err := os.Symlink(outside, filepath.Join(app, "Contents/link")); err != nil {
		t.Fatal(err)
	}
	setQuarantine(t, app, "0181;5f000000;;NEW")
	setQuarantine(t, filepath.Join(app, "Contents/Info.plist"), "0181;5f000000;;NEW")
	if err := inheritApproval(context.Background(), app); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{app, filepath.Join(app, "Contents"), filepath.Join(app, "Contents/Info.plist")} {
		if got := readQuarantine(context.Background(), p); got != "01c1;5f000000;;NEW" {
			t.Errorf("%s quarantine = %q", p, got)
		}
	}
	if got := readQuarantine(context.Background(), outside); got != "" {
		t.Errorf("followed the symlink: %s quarantine = %q", outside, got)
	}
}
