package cask

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func infoPlist(short, version string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>CFBundleShortVersionString</key><string>` + short + `</string>
<key>CFBundleVersion</key><string>` + version + `</string>
</dict></plist>
`
}

func versioned(t *testing.T) *testInstall {
	t.Helper()
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("needs plutil")
	}
	var tt testing.T
	body := tarGzBytes(&tt,
		entry{name: "App.app/Contents/Info.plist", body: infoPlist("1.0", "100")},
		entry{name: "App.app/Contents/MacOS/app", body: "#!/bin/sh\n", mode: 0o755},
		entry{name: "tool", body: "#!/bin/sh\n"})
	ti := newTestInstall(t, body)
	ti.in.Adopt = true
	return ti
}

func existingApp(t *testing.T, ti *testInstall, plist string) string {
	t.Helper()
	app := filepath.Join(ti.appDir, "App.app")
	writeFile(t, filepath.Join(app, "Contents/Info.plist"), []byte(plist), 0o644)
	writeFile(t, filepath.Join(app, "Contents/MacOS/app"), []byte("#!/bin/sh\n"), 0o755)
	return app
}

func TestInstallAdoptsAnExistingApp(t *testing.T) {
	ti := versioned(t)
	app := existingApp(t, ti, infoPlist("1.0", "100"))
	writeFile(t, filepath.Join(app, "Contents/Resources/mine"), []byte("kept"), 0o644)
	if err := ti.in.Install(context.Background(), ti.cask(t, appArtifacts)); err != nil {
		t.Fatal(err)
	}
	if !exists(filepath.Join(app, "Contents/Resources/mine")) {
		t.Error("the existing app was replaced")
	}
	if got := readLink(t, filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")); got != app {
		t.Errorf("Caskroom link -> %s, want %s", got, app)
	}
	wantOut := "==> Installing Cask app\n" +
		"==> Adopting existing App at '" + app + "'\n" +
		"==> Linking Binary 'app' to '" + filepath.Join(ti.prefix, "bin/app") + "'\n" +
		"==> Linking Binary 'tool' to '" + filepath.Join(ti.prefix, "bin/tool") + "'\n" +
		"🍺  app was successfully installed!\n"
	if got := ti.out.String(); got != wantOut {
		t.Errorf("output =\n%s\nwant\n%s", got, wantOut)
	}
}

func TestInstallRefusesToAdoptADifferentVersion(t *testing.T) {
	for _, tc := range []struct {
		short, version, want string
	}{
		{"2.0", "100", "The bundle short version of %s is 1.0 but is 2.0 for %s!"},
		{"1.0", "101", "The bundle version of %s is 100 but is 101 for %s!"},
	} {
		ti := versioned(t)
		app := existingApp(t, ti, infoPlist(tc.short, tc.version))
		err := ti.in.Install(context.Background(), ti.cask(t, appArtifacts))
		if want := "app: It seems the existing App is different from the one being installed."; err == nil || err.Error() != want {
			t.Errorf("err = %v, want %s", err, want)
		}
		staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")
		if want := "Error: " + strings.ReplaceAll(strings.Replace(tc.want, "%s", staged, 1), "%s", app) + "\n"; ti.warn.String() != want {
			t.Errorf("stderr = %q, want %q", ti.warn, want)
		}
		if !exists(filepath.Join(app, "Contents/Info.plist")) {
			t.Error("the existing app was removed")
		}
		if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
			t.Error("Caskroom/app left behind")
		}
	}
}

func TestInstallAdoptsAnAutoUpdatingAppOfAnyVersion(t *testing.T) {
	ti := versioned(t)
	app := existingApp(t, ti, infoPlist("3.0", "300"))
	c := ti.cask(t, appArtifacts)
	c.AutoUpdates = true
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := readLink(t, filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")); got != app {
		t.Errorf("Caskroom link -> %s", got)
	}
}

const versionlessPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0"><dict/></plist>
`

var versionlessTarball = func() []byte {
	var t testing.T
	return tarGzBytes(&t,
		entry{name: "App.app/Contents/Info.plist", body: versionlessPlist},
		entry{name: "App.app/Contents/MacOS/app", body: "#!/bin/sh\n", mode: 0o755},
		entry{name: "tool", body: "#!/bin/sh\n"})
}()

func TestInstallAdoptsByComparingFilesWithoutVersions(t *testing.T) {
	for _, tc := range []struct {
		body string
		ok   bool
	}{{"#!/bin/sh\n", true}, {"changed\n", false}} {
		ti := newTestInstall(t, versionlessTarball)
		ti.in.Adopt = true
		app := existingApp(t, ti, versionlessPlist)
		writeFile(t, filepath.Join(app, "Contents/MacOS/app"), []byte(tc.body), 0o755)
		err := ti.in.Install(context.Background(), ti.cask(t, `[{"app":["App.app"]}]`))
		if tc.ok && err != nil {
			t.Errorf("%q: %v", tc.body, err)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "is different from the one being installed")) {
			t.Errorf("%q: err = %v", tc.body, err)
		}
	}
}

func TestInstallTakesOverTheBinaryLinksOfAnAdoptedApp(t *testing.T) {
	ti := versioned(t)
	app := existingApp(t, ti, infoPlist("1.0", "100"))
	bin := filepath.Join(ti.prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/tool")
	for name, to := range map[string]string{
		"app": filepath.Join(app, "Contents/MacOS/app"), "tool": staged,
		"old": filepath.Join(ti.prefix, "Caskroom/app/0.9/tool"),
	} {
		if err := os.Symlink(to, filepath.Join(bin, name)); err != nil {
			t.Fatal(err)
		}
	}
	c := ti.cask(t, `[{"app":["App.app"]},{"binary":["$APPDIR/App.app/Contents/MacOS/app"]},{"binary":["tool"]},{"binary":["tool",{"target":"old"}]}]`)
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tool", "old"} {
		if got := readLink(t, filepath.Join(bin, name)); got != staged {
			t.Errorf("bin/%s -> %s", name, got)
		}
	}
	want := "Warning: It seems there is already a Binary at '" + filepath.Join(bin, "app") + "'; overwriting.\n" +
		"Warning: It seems there is already a Binary at '" + filepath.Join(bin, "tool") + "'; overwriting.\n"
	if ti.warn.String() != want {
		t.Errorf("warnings = %q, want %q", ti.warn, want)
	}
}

func TestInstallLeavesALinkThatResolvesOnceStaged(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	tool := filepath.Join(ti.prefix, "bin/tool")
	if err := os.MkdirAll(filepath.Dir(tool), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(ti.prefix, "Caskroom/app/1.0/tool"), tool); err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"binary":["tool"]}]`)); err != nil {
		t.Fatal(err)
	}
	if want := "==> Binary 'tool' is already linked to '" + tool + "'\n"; !strings.Contains(ti.out.String(), want) {
		t.Errorf("output is missing %q:\n%s", want, ti.out)
	}
}

func TestInstallKeepsAnAdoptedAppWhenItFails(t *testing.T) {
	ti := versioned(t)
	app := existingApp(t, ti, infoPlist("1.0", "100"))
	err := ti.in.Install(context.Background(), ti.cask(t, `[{"app":["App.app"]},{"binary":["missing"]}]`))
	if err == nil {
		t.Fatal("installed with a missing binary")
	}
	if !exists(filepath.Join(app, "Contents/Info.plist")) {
		t.Error("the adopted app was removed")
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("Caskroom/app left behind")
	}
}
