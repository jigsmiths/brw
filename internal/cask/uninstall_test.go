package cask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/sudo"
)

type fakeExec struct {
	calls   []string
	runs    []sudo.Command
	running map[string]int
	fail    map[string]bool
	noFix   bool
	handle  map[string]func(args []string) ([]byte, error)
	// gui is whether the user has a console session.
	gui bool
	// hostID is the bundle ID of the app brw runs in; runningIDs lists the running apps.
	hostID, runningIDs string
}

// run is the sudo runner that performs privileged commands through exec as /usr/bin/sudo calls.
func (f *fakeExec) run(ctx context.Context, c sudo.Command) (sudo.Result, error) {
	f.runs = append(f.runs, c)
	exe, args := c.Exe, c.Args
	if exe == "/usr/bin/sudo" {
		return sudo.Result{}, nil
	}
	if exe == "/usr/bin/xargs" {
		exe, args = args[2], append(slices.Clone(args[3:]), strings.Split(c.Input, "\x00")...)
	}
	prefix := []string{"-E", "--"}
	switch c.Privilege {
	case sudo.User:
		out, err := f.exec(ctx, exe, args...)
		return sudo.Result{Stdout: string(out)}, err
	case sudo.Root:
		prefix = append([]string{"-u", "root"}, prefix...)
	}
	out, err := f.exec(ctx, "/usr/bin/sudo", slices.Concat(prefix, []string{exe}, args)...)
	return sudo.Result{Stdout: string(out)}, err
}

func (f *fakeExec) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, strings.TrimSpace(name+" "+strings.Join(args, " ")))
	if f.fail[name] {
		return nil, errors.New("failed")
	}
	if h := f.handle[name]; h != nil {
		return h(args)
	}
	switch name {
	case "/usr/bin/sudo":
		cmd := args[slices.Index(args, "--")+1:]
		target := cmd[len(cmd)-1]
		if cmd[0] == "chown" {
			_ = exec.CommandContext(ctx, "chflags", "-R", "0", target).Run()
			return exec.CommandContext(ctx, "chmod", "-R", "u+rwx", target).Output()
		}
		dir := filepath.Dir(target)
		if fi, err := os.Stat(dir); err == nil {
			_ = os.Chmod(dir, 0o755)
			defer func() { _ = os.Chmod(dir, fi.Mode().Perm()) }()
		}
		return exec.CommandContext(ctx, cmd[0], cmd[1:]...).Output()
	case "/usr/bin/chflags", "/bin/chmod":
		if f.noFix {
			return nil, nil
		}
		return exec.CommandContext(ctx, name, args...).Output()
	case "/bin/cp", "/bin/mkdir":
		return exec.CommandContext(ctx, name, args...).Output()
	case "/bin/ps":
		if args[1] == "ppid=" {
			return []byte("1\n"), nil
		}
		return []byte(fmt.Sprintf("%d\n", os.Getuid())), nil
	case "/usr/bin/lsappinfo":
		switch args[0] {
		case "find":
			if f.hostID != "" {
				return []byte("ASN:0x0-0x1-\"Host\":\n"), nil
			}
			return []byte("\n"), nil
		case "info":
			return []byte("[ NULL ]  ASN:0x0-0x1: \n    bundleID=\"" + f.hostID + "\"\n"), nil
		case "list":
			var b strings.Builder
			for i, id := range strings.Fields(f.runningIDs) {
				fmt.Fprintf(&b, " %d) \"app\" ASN:0x0-0x%d: \n    bundleID=\"%s\"\n", i+1, i+1, id)
			}
			return []byte(b.String()), nil
		}
	case "who":
		if f.gui {
			return []byte(loginName(&testing.T{}) + " console Nov 25 10:00\n"), nil
		}
		return nil, nil
	case "osascript":
		if args[0] == "-e" {
			return nil, nil
		}
		id := args[len(args)-1]
		if !strings.Contains(args[3], "app.quit()") {
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
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}, gui: true}
	ti.in.Exec, ti.in.Run = f.exec, f.run
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
	f.hostID = "com.example.app"
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

func TestUninstallDeleteSudoGoesThroughTheRunnerAsBrewBuildsIt(t *testing.T) {
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
	c := f.runs[len(f.runs)-1]
	if c.Exe != "/usr/bin/xargs" || c.Privilege != sudo.Sudo || !c.MustSucceed || c.Input != filepath.Join(home, "locked") ||
		!slices.Equal(c.Args, []string{"-0", "--", "/bin/rm", "-r", "-f", "--"}) {
		t.Errorf("runner got %v", c)
	}
}

func TestUninstallSystemLaunchctlServiceRunsAsRoot(t *testing.T) {
	ti, f := installed(t, `[{"uninstall":[{"launchctl":"com.example.daemon"}]},{"app":["App.app"]}]`)
	f.handle = map[string]func([]string) ([]byte, error){"/bin/launchctl": func(args []string) ([]byte, error) {
		if args[0] == "print" && args[1] == "system/com.example.daemon" {
			return nil, nil
		}
		return nil, errors.New("not found")
	}}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	var removes []sudo.Command
	for _, c := range f.runs {
		if c.Exe == "/bin/launchctl" {
			removes = append(removes, c)
		}
	}
	if len(removes) != 1 || removes[0].Privilege != sudo.Root || !slices.Equal(removes[0].Args, []string{"remove", "com.example.daemon"}) {
		t.Errorf("launchctl runs = %v", removes)
	}
}

func TestUninstallSkipsSystemLaunchctlWhenSudoIsOff(t *testing.T) {
	ti, f := installed(t, `[{"uninstall":[{"launchctl":"com.example.daemon"}]},{"app":["App.app"]}]`)
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if got := f.ran("/bin/launchctl print system/"); len(got) != 0 {
		t.Errorf("system domain was queried: %q", got)
	}
}

func TestInstallIntoALockedAppFolderNeedsSudoAvailable(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}}
	ti.in.Exec, ti.in.Run = f.exec, f.run
	lock(t, ti.appDir)
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"app":["App.app"]}]`)); err == nil {
		t.Error("the install should fail without sudo")
	}
	if got := f.ran("/usr/bin/sudo"); len(got) != 0 {
		t.Errorf("sudo ran: %q", got)
	}
}

func TestUninstallRefusesWhatBrwCannotUndo(t *testing.T) {
	for _, artifacts := range []string{
		`[{"uninstall":[{"bogus":"x"}]},{"app":["App.app"]}]`,
		`[{"app":["App.app"]},{"suite":["App.app/man"]}]`,
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

func lock(t *testing.T, dir string) {
	t.Helper()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
}

func TestUninstallFromALockedAppFolder(t *testing.T) {
	ti, f := installed(t, `[{"app":["App.app"]}]`)
	lock(t, ti.appDir)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(ti.appDir, "App.app")
	staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/App.app")
	want := "==> Uninstalling Cask app\n==> Backing up App 'App.app' to '" + staged + "'\n==> Removing App '" + app + "'\n" +
		"==> Purging files for version 1.0 of Cask app\n"
	if got := ti.out.String(); got != want {
		t.Errorf("output:\n%s\nwant:\n%s", got, want)
	}
	if exists(app) || exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("the app and the Caskroom entry should be gone")
	}
	if got := f.ran("/usr/bin/sudo"); !slices.Equal(got, []string{"/usr/bin/sudo -E -- /bin/rm -R -f -- " + app}) {
		t.Errorf("sudo calls = %q", got)
	}
}

func lockedFile(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("chflags is macOS's")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("chflags", "uchg", path).Run(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("chflags", "nouchg", path).Run() })
}

func TestPurgeRemovesFilesItMustUnlockFirst(t *testing.T) {
	ti, _ := installed(t, `[{"app":["App.app"]}]`)
	version := filepath.Join(ti.prefix, "Caskroom/app/1.0")
	lockedFile(t, filepath.Join(version, "locked"))
	if err := os.MkdirAll(filepath.Join(version, "ro/sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	lock(t, filepath.Join(version, "ro"))
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("the Caskroom entry should be gone")
	}
	if strings.Contains(ti.out.String(), "sudo") {
		t.Errorf("no sudo needed: %s", ti.out.String())
	}
}

func TestPurgeTakesOwnershipWithSudo(t *testing.T) {
	ti, f := installed(t, `[{"app":["App.app"]}]`)
	version := filepath.Join(ti.prefix, "Caskroom/app/1.0")
	lockedFile(t, filepath.Join(version, "locked"))
	f.noFix = true
	ownedByOther = func(string, bool) bool { return true }
	t.Cleanup(func() { ownedByOther = ownedByAnother })
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ti.out.String(), "==> Using sudo to gain ownership of path '"+version+"'\n") {
		t.Errorf("output:\n%s", ti.out.String())
	}
	if got := f.ran("/usr/bin/sudo -E -- chown"); len(got) != 1 || !strings.HasSuffix(got[0], " -- "+currentUser()+" "+version) {
		t.Errorf("chown calls = %q", got)
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("the Caskroom entry should be gone")
	}
}

func TestUninstallDeletesWhatOnlySudoCanAndLeavesSystemFolders(t *testing.T) {
	home := t.TempDir()
	ti, f := installed(t, `[{"uninstall":[{"delete":["~/locked/file","~/Library"]}]},{"app":["App.app"]}]`)
	ti.in.Where.Home = home
	for _, d := range []string{"locked", "Library"} {
		if err := os.Mkdir(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, "locked/file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	lock(t, filepath.Join(home, "locked"))
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if exists(filepath.Join(home, "locked/file")) || !exists(filepath.Join(home, "Library")) {
		t.Error("the locked file should go and ~/Library stay")
	}
	if got, want := ti.warn.String(), "Warning: Skipping delete for undeletable path '"+filepath.Join(home, "Library")+"'.\n"; got != want {
		t.Errorf("warnings = %q, want %q", got, want)
	}
	if got := f.ran("/usr/bin/sudo"); len(got) != 1 {
		t.Errorf("sudo calls = %q", got)
	}
}

const loginItemArtifacts = `[{"uninstall":[{"login_item":%s}]},{"app":["App.app"]}]`

func installedWithLoginItem(t *testing.T, names string) (*testInstall, *fakeExec, *[]time.Duration) {
	t.Helper()
	ti, f := installed(t, fmt.Sprintf(loginItemArtifacts, names))
	var slept []time.Duration
	ti.in.Sleep = func(d time.Duration) { slept = append(slept, d) }
	return ti, f, &slept
}

func TestUninstallRemovesLoginItem(t *testing.T) {
	ti, f, slept := installedWithLoginItem(t, `"Example"`)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	app := filepath.Join(ti.appDir, "App.app")
	want := []string{
		`osascript -e tell application "System Events" to delete every login item whose path is "` + app + `"`,
		`osascript -e tell application "System Events" to delete every login item whose name is "Example"`,
	}
	if got := f.ran("osascript"); !slices.Equal(got, want) {
		t.Errorf("osascript calls = %q, want %q", got, want)
	}
	if got, want := ti.out.String(), "==> Removing login item "+app+"\n==> Removing login item Example\n"; !strings.Contains(got, want) {
		t.Errorf("output = %q, want it to contain %q", got, want)
	}
	if !slices.Equal(*slept, []time.Duration{time.Second, time.Second}) {
		t.Errorf("sleeps = %v", *slept)
	}
}

func TestUninstallLoginItemsOnePathPerApp(t *testing.T) {
	ti, f, slept := installedWithLoginItem(t, `"Example"`)
	apps := []string{"/Applications/One.app", "/Applications/Two.app"}
	ti.in.loginItems(context.Background(), apps, nil)
	want := []string{
		`osascript -e tell application "System Events" to delete every login item whose path is "/Applications/One.app"`,
		`osascript -e tell application "System Events" to delete every login item whose path is "/Applications/Two.app"`,
	}
	if got := f.ran("osascript"); !slices.Equal(got, want) {
		t.Errorf("osascript calls = %q, want %q", got, want)
	}
	if len(*slept) != 2 {
		t.Errorf("sleeps = %v", *slept)
	}
}

func TestUninstallWithoutLoginItemDirectiveRunsNone(t *testing.T) {
	ti, f := installed(t, `[{"uninstall":[{"delete":"~/gone"}]},{"app":["App.app"]}]`)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if got := f.ran("osascript"); len(got) != 0 {
		t.Errorf("osascript calls = %q", got)
	}
}

func TestUninstallLoginItemPathFailureWarns(t *testing.T) {
	ti, f, _ := installedWithLoginItem(t, `"Example"`)
	app := filepath.Join(ti.appDir, "App.app")
	f.handle = map[string]func([]string) ([]byte, error){"osascript": func(args []string) ([]byte, error) {
		if strings.Contains(args[1], "whose path is") {
			return nil, errors.New("failed")
		}
		return nil, nil
	}}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: Removal of login item " + app + " failed. " + automationAccess + "\n"; ti.warn.String() != want {
		t.Errorf("warnings = %q, want %q", ti.warn.String(), want)
	}
	if got := f.ran("osascript"); len(got) != 2 {
		t.Errorf("osascript calls = %q", got)
	}
}

func TestUninstallLoginItemFailureWarns(t *testing.T) {
	ti, f, _ := installedWithLoginItem(t, `"Example"`)
	f.fail["osascript"] = true
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ti.warn.String(), automationAccess) {
		t.Errorf("warnings = %q", ti.warn.String())
	}
	if exists(filepath.Join(ti.appDir, "App.app")) {
		t.Error("the app should still be removed")
	}
}

func TestUninstallLoginItemQuotesTheName(t *testing.T) {
	ti, f, _ := installedWithLoginItem(t, `"Ex\"ample\\"`)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	want := `osascript -e tell application "System Events" to delete every login item whose name is "Ex\"ample\\"`
	got := f.ran("osascript")
	if len(got) != 2 || got[1] != want {
		t.Errorf("osascript calls = %q, want last %q", got, want)
	}
}

func TestUninstallSkipsLoginItemWhenUpgrading(t *testing.T) {
	ti, f, slept := installedWithLoginItem(t, `"Example"`)
	ti.in.upgrading = true
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if got := f.ran("osascript"); len(got) != 0 {
		t.Errorf("osascript calls = %q", got)
	}
	if strings.Contains(ti.out.String(), "login item") || len(*slept) != 0 {
		t.Errorf("output = %q, sleeps = %v", ti.out.String(), *slept)
	}
}
