package cask

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeExec records commands. osascript reports apps as running while
// running says so; rm and rmdir run for real; anything else succeeds.
type fakeExec struct {
	calls   []string
	running map[string]int // bundle ID: how many more checks find it running
	fail    map[string]bool
}

func (f *fakeExec) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	if f.fail[name] {
		return nil, errors.New("failed")
	}
	switch name {
	case "osascript":
		id := args[len(args)-1]
		if !strings.Contains(args[3], "app.quit()") { // the running check
			if f.running[id] > 0 {
				f.running[id]--
				return nil, nil
			}
			return nil, errors.New("not running")
		}
		return nil, nil
	case "/bin/rm", "/bin/rmdir":
		return exec.CommandContext(ctx, name, args...).Output()
	case "/bin/launchctl":
		if args[0] == "print" {
			return nil, errors.New("not found")
		}
	}
	return nil, nil
}

func (f *fakeExec) ran(prefix string) []string {
	var out []string
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func installed(t *testing.T, artifacts string) (*testInstall, *fakeExec) {
	t.Helper()
	ti := newTestInstall(t, appTarball)
	if err := ti.in.Install(context.Background(), ti.cask(t, artifacts)); err != nil {
		t.Fatal(err)
	}
	ti.out.Reset()
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}}
	ti.in.Exec = f.exec
	return ti, f
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func TestUninstallAppAndBinaries(t *testing.T) {
	ti, f := installed(t, appArtifacts)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(ti.appDir, "App.app")
	staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")
	want := strings.Join([]string{
		"==> Uninstalling Cask app",
		"==> Backing up App 'App.app' to '" + staged + "'",
		"==> Removing App '" + app + "'",
		"==> Unlinking Binary '" + filepath.Join(ti.prefix, "bin/app") + "'",
		"==> Unlinking Binary '" + filepath.Join(ti.prefix, "bin/tool") + "'",
		"==> Purging files for version 1.0 of Cask app",
	}, "\n") + "\n"
	if got := ti.out.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	for _, p := range []string{app, filepath.Join(ti.prefix, "Caskroom/app"), filepath.Join(ti.prefix, "bin/app"), filepath.Join(ti.prefix, "bin/tool")} {
		if exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if !exists(filepath.Join(ti.prefix, "Caskroom")) || !exists(filepath.Join(ti.prefix, "bin")) {
		t.Error("the Caskroom and bin should stay")
	}
	// The app was not running, so it was only checked.
	if got := f.ran("osascript"); len(got) != 1 {
		t.Errorf("osascript calls = %q", got)
	}
}

func TestUninstallQuitsARunningApp(t *testing.T) {
	ti, f := installed(t, appArtifacts)
	f.running["com.example.app"] = 1
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(ti.out.String(), "\n")
	want := []string{"==> Uninstalling Cask app", "==> Quitting application 'com.example.app'...", "Application 'com.example.app' quit successfully."}
	if !slices.Equal(lines[:3], want) {
		t.Errorf("output starts %q, want %q", lines[:3], want)
	}
}

func TestUninstallLeavesTheAppBrwRunsIn(t *testing.T) {
	ti, f := installed(t, appArtifacts)
	t.Setenv("__CFBundleIdentifier", "com.example.app")
	f.running["com.example.app"] = 1
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if got := ti.warn.String(); !strings.Contains(got, "Warning: Skipping quitting application 'com.example.app' as `brw` is running inside it.") {
		t.Errorf("warnings = %q", got)
	}
	if strings.Contains(ti.out.String(), "Quitting") {
		t.Errorf("output = %q", ti.out.String())
	}
}

func TestUninstallDirectives(t *testing.T) {
	home := t.TempDir()
	ti, f := installed(t, `[
		{"uninstall":[{"launchctl":"com.example.helper","delete":["~/gone","~/missing*","relative"],"trash":"~/old","rmdir":"~/Empty"}]},
		{"app":["App.app"]}]`)
	ti.in.Where.Home = home
	for _, d := range []string{"gone/sub", "old", "Empty/a/b"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "Empty/a/.DS_Store"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	out := ti.out.String()
	for _, want := range []string{
		"==> Removing launchctl service com.example.helper\n",
		"==> Removing files:\n~/gone\n~/missing*\n==> Trashing files:\n~/old\n==> Removing directories if empty:\n~/Empty\n~/Empty/a\n~/Empty/a/b\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got := ti.warn.String(); got != "Warning: Skipping delete for relative path 'relative'.\n" {
		t.Errorf("warnings = %q", got)
	}
	if exists(filepath.Join(home, "gone")) || exists(filepath.Join(home, "Empty")) {
		t.Error("delete and rmdir should have removed their paths")
	}
	if got := f.ran("/usr/bin/trash"); !slices.Equal(got, []string{"/usr/bin/trash " + filepath.Join(home, "old")}) {
		t.Errorf("trash calls = %q", got)
	}
	if got := f.ran("/usr/bin/sudo"); len(got) != 0 {
		t.Errorf("sudo should not be needed: %q", got)
	}
}

func TestUninstallDeleteFallsBackToSudo(t *testing.T) {
	home := t.TempDir()
	ti, f := installed(t, `[{"uninstall":[{"delete":"~/locked"}]},{"app":["App.app"]}]`)
	ti.in.Where.Home = home
	if err := os.WriteFile(filepath.Join(home, "locked"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.fail["/bin/rm"] = true
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/sudo -E -- /bin/rm -r -f -- " + filepath.Join(home, "locked")}
	if got := f.ran("/usr/bin/sudo"); !slices.Equal(got, want) {
		t.Errorf("sudo calls = %q, want %q", got, want)
	}
}

func TestUninstallRefusesWhatBrwCannotUndo(t *testing.T) {
	for _, artifacts := range []string{
		`[{"uninstall":[{"pkgutil":"com.example.pkg"}]},{"app":["App.app"]}]`,
		`[{"app":["App.app"]},{"manpage":["App.app/man/app.1"]}]`,
	} {
		ti, f := installed(t, `[{"app":["App.app"]}]`)
		receipt := filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")
		if err := os.WriteFile(receipt, []byte(`{"uninstall_artifacts":`+artifacts+`}`), 0o644); err != nil {
			t.Fatal(err)
		}
		err := ti.in.Uninstall(context.Background(), "app", "1.0", false)
		if !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", artifacts, err)
		}
		if ti.out.Len() > 0 || len(f.calls) > 0 || !exists(filepath.Join(ti.appDir, "App.app")) {
			t.Errorf("%s: something changed: %q %q", artifacts, ti.out.String(), f.calls)
		}
	}
}

func TestUninstallAppAlreadyGone(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]}]`)
	if err := os.RemoveAll(filepath.Join(ti.appDir, "App.app")); err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	want := "==> Uninstalling Cask app\n==> Purging files for version 1.0 of Cask app\n"
	if got := ti.out.String(); got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
}

func TestUninstallAppInTheWay(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]}]`)
	staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")
	if err := os.Remove(staged); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(staged, 0o755); err != nil {
		t.Fatal(err)
	}
	err := ti.in.Uninstall(context.Background(), "app", "1.0", false)
	if err == nil || !strings.Contains(err.Error(), "It seems there is already an App at '"+staged+"'.") {
		t.Fatalf("err = %v", err)
	}
	if !exists(filepath.Join(ti.appDir, "App.app")) {
		t.Error("the app should stay")
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", true); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(ti.appDir, "App.app")) || exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("--force should remove both")
	}
}

func TestUninstallLeavesAFormulasBinary(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]},{"binary":["tool"]}]`)
	keg := filepath.Join(ti.prefix, "Cellar/tool/1.0/bin")
	if err := os.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(keg, "tool"), nil, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(ti.prefix, "bin/tool")
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/tool/1.0/bin/tool", link); err != nil {
		t.Fatal(err)
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if !exists(link) {
		t.Error("the formula's link should stay")
	}
}
