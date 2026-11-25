package cask

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func withDirectives(t *testing.T, directives string) (*testInstall, *fakeExec, *fakeRun) {
	t.Helper()
	ti, f := installed(t, `[{"app":["App.app"]}]`)
	receipt := filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json")
	if err := os.WriteFile(receipt, []byte(`{"uninstall_artifacts":[{"uninstall":[`+directives+`]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	r := &fakeRun{}
	ti.in.Run = r.run
	return ti, f, r
}

func stagedFile(t *testing.T, ti *testInstall, name string) string {
	t.Helper()
	return writeFile(t, filepath.Join(ti.prefix, "Caskroom/app/1.0", name), []byte("#!/bin/sh\n"), 0o644)
}

func TestUninstallRunsEarlyScriptThenScript(t *testing.T) {
	ti, _, r := withDirectives(t, `{"script":{"executable":"uninstall.sh","args":["--yes"],"input":["x","y"]},"early_script":{"executable":"early.sh","args":["a"]}}`)
	early, script := stagedFile(t, ti, "early.sh"), stagedFile(t, ti, "uninstall.sh")
	var modes []os.FileMode
	r.handle = func(c Command) ([]byte, error) {
		fi, err := os.Stat(c.Args[slices.IndexFunc(c.Args, func(a string) bool { return strings.HasSuffix(a, ".sh") })])
		if err != nil {
			return nil, err
		}
		modes = append(modes, fi.Mode().Perm())
		return nil, nil
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 {
		t.Fatalf("calls = %+v", r.calls)
	}
	if c := r.calls[0]; c.Name != "/usr/bin/env" || !slices.Equal(c.Args, []string{early, "a"}) || c.Tee == nil {
		t.Errorf("early_script = %+v", c)
	}
	if c := r.calls[1]; c.Name != "/usr/bin/env" || !slices.Equal(c.Args, []string{script, "--yes"}) || string(c.Stdin) != "xy" {
		t.Errorf("script = %+v", c)
	}
	for i, m := range modes {
		if m&0o111 == 0 {
			t.Errorf("script %d should be made executable, mode %v", i, m)
		}
	}
	want := "==> Running uninstall script early.sh\n==> Running uninstall script uninstall.sh\n"
	if !strings.Contains(ti.out.String(), want) {
		t.Errorf("out = %q", ti.out.String())
	}
}

func TestUninstallRefusesAScriptThatNeedsSudo(t *testing.T) {
	ti, _, r := withDirectives(t, `{"script":{"executable":"s.sh","sudo":true}}`)
	stagedFile(t, ti, "s.sh")
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
	if len(r.calls) > 0 || ti.out.Len() > 0 {
		t.Errorf("something ran: %+v %q", r.calls, ti.out.String())
	}
}

func TestUninstallScriptAsAString(t *testing.T) {
	ti, _, r := withDirectives(t, `{"script":"uninstall.sh"}`)
	script := stagedFile(t, ti, "uninstall.sh")
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || !slices.Equal(r.calls[0].Args, []string{script}) {
		t.Errorf("calls = %+v", r.calls)
	}
}

func TestUninstallScriptThatIsNotThere(t *testing.T) {
	ti, _, r := withDirectives(t, `{"script":{"executable":"nope.sh"}}`)
	err := ti.in.Uninstall(context.Background(), "app", "1.0", false)
	if want := "app: uninstall script nope.sh does not exist."; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(r.calls) > 0 {
		t.Errorf("ran %+v", r.calls)
	}
}

func TestUninstallScriptThatIsNotThereIsSkippedWhenForced(t *testing.T) {
	ti, _, r := withDirectives(t, `{"script":{"executable":"/nonexistent/nope.sh"}}`)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", true); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: uninstall script /nonexistent/nope.sh does not exist; skipping.\n"; ti.warn.String() != want {
		t.Errorf("warn = %q, want %q", ti.warn.String(), want)
	}
	if len(r.calls) > 0 {
		t.Errorf("ran %+v", r.calls)
	}
}

func TestUninstallScriptFoundOnThePath(t *testing.T) {
	ti, _, r := withDirectives(t, `{"script":{"executable":"ls"}}`)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 1 || !slices.Equal(r.calls[0].Args, []string{"ls"}) {
		t.Errorf("calls = %+v", r.calls)
	}
}

func TestUninstallScriptWithoutAnExecutable(t *testing.T) {
	ti, _, r := withDirectives(t, `{"script":{"args":["x"]}}`)
	err := ti.in.Uninstall(context.Background(), "app", "1.0", false)
	if want := "Cask 'app' definition is invalid: uninstall :script without :executable."; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
	if !strings.Contains(ti.out.String(), "==> Running uninstall script \n") || len(r.calls) > 0 {
		t.Errorf("out = %q, calls = %+v", ti.out.String(), r.calls)
	}
}

func TestUninstallScriptWarnsAboutArguments(t *testing.T) {
	ti, _, r := withDirectives(t, `{"early_script":{"executable":"s.sh","bogus":1,"print_stdout":false}}`)
	stagedFile(t, ti, "s.sh")
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	want := "Warning: Unknown arguments to uninstall :early_script -- [:bogus] (ignored). Running `brew update; brew cleanup` will likely fix it.\n" +
		"Error: Some arguments to uninstall :early_script will be ignored -- :[:bogus] (overridden).\n"
	if ti.warn.String() != want {
		t.Errorf("warn = %q\nwant %q", ti.warn.String(), want)
	}
	if len(r.calls) != 1 || r.calls[0].Tee == nil {
		t.Errorf("print_stdout should be forced on: %+v", r.calls)
	}
}

func TestUninstallScriptThatFails(t *testing.T) {
	ti, _, r := withDirectives(t, `{"script":{"executable":"s.sh"}}`)
	stagedFile(t, ti, "s.sh")
	r.handle = func(Command) ([]byte, error) { return nil, errors.New("exited with 1") }
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err == nil {
		t.Error("a failing script should fail the uninstall")
	}
}

func signalling(t *testing.T, directives string) (*testInstall, *fakeExec, *[]string) {
	t.Helper()
	ti, f, _ := withDirectives(t, directives)
	f.handle = map[string]func([]string) ([]byte, error){
		"/bin/launchctl": func(args []string) ([]byte, error) {
			if args[0] == "list" {
				return []byte("PID\tStatus\tLabel\n123\t0\tapplication.com.example.app.1234.5678\n0\t0\tcom.example.app\n456\t0\tcom.example.apple\n789\t0\tcom.other\n"), nil
			}
			return nil, errors.New("not found")
		},
	}
	var kills []string
	ti.in.Kill = func(pid int, sig syscall.Signal) error {
		kills = append(kills, fmt.Sprintf("%d:%d", pid, sig))
		return nil
	}
	return ti, f, &kills
}

func TestUninstallSignalsRunningProcesses(t *testing.T) {
	ti, _, kills := signalling(t, `{"signal":["TERM","com.example.app","KILL","com.other"]}`)
	var slept []string
	ti.in.Sleep = func(d time.Duration) { slept = append(slept, d.String()) }
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := []string{fmt.Sprintf("123:%d", syscall.SIGTERM), fmt.Sprintf("789:%d", syscall.SIGKILL)}; !slices.Equal(*kills, want) {
		t.Errorf("kills = %v, want %v", *kills, want)
	}
	if want := []string{"3s", "3s"}; !slices.Equal(slept, want) {
		t.Errorf("sleeps = %v", slept)
	}
	want := "==> Signalling 'TERM' to application ID 'com.example.app'\n==> Signalling 'KILL' to application ID 'com.other'\n"
	if !strings.Contains(ti.out.String(), want) {
		t.Errorf("out = %q", ti.out.String())
	}
}

func TestUninstallSignalAcceptsPairsAndSigPrefix(t *testing.T) {
	ti, _, kills := signalling(t, `{"signal":[["SIGHUP","com.other"]]}`)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := []string{fmt.Sprintf("789:%d", syscall.SIGHUP)}; !slices.Equal(*kills, want) {
		t.Errorf("kills = %v", *kills)
	}
}

func TestUninstallSignalSkipsOtherUsersProcesses(t *testing.T) {
	ti, f, kills := signalling(t, `{"signal":["TERM","com.other"]}`)
	f.handle["/bin/ps"] = func(args []string) ([]byte, error) {
		if args[1] == "ppid=" {
			return []byte("1\n"), nil
		}
		return []byte("0\n"), nil
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if len(*kills) > 0 || strings.Contains(ti.out.String(), "Signalling") {
		t.Errorf("kills = %v, out = %q", *kills, ti.out.String())
	}
}

func TestUninstallSignalSkipsTheHostApp(t *testing.T) {
	ti, f, kills := signalling(t, `{"signal":["TERM","com.other"]}`)
	f.hostID = "COM.other"
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: Skipping signalling application 'com.other' as `brw` is running inside it.\n"; ti.warn.String() != want || len(*kills) > 0 {
		t.Errorf("warn = %q, kills = %v", ti.warn.String(), *kills)
	}
}

func TestUninstallSignalWarnsWhenNotPermitted(t *testing.T) {
	ti, _, _ := signalling(t, `{"signal":["TERM","com.other"]}`)
	ti.in.Kill = func(int, syscall.Signal) error { return syscall.EPERM }
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: Failed to kill com.other PIDs 789 with signal TERM: Operation not permitted\n"; ti.warn.String() != want {
		t.Errorf("warn = %q", ti.warn.String())
	}
}

func TestUninstallSignalNeedsPairs(t *testing.T) {
	ti, _, kills := signalling(t, `{"signal":["TERM","com.other","KILL"]}`)
	err := ti.in.Uninstall(context.Background(), "app", "1.0", false)
	if want := "Cask 'app' definition is invalid: Each uninstall :signal must consist of 2 elements."; err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("err = %v", err)
	}
	if len(*kills) > 0 {
		t.Errorf("kills = %v", *kills)
	}
}

func TestUninstallSignalRejectsUnknownSignals(t *testing.T) {
	ti, _, kills := signalling(t, `{"signal":["BOGUS","com.other"]}`)
	err := ti.in.Uninstall(context.Background(), "app", "1.0", false)
	if err == nil || !strings.Contains(err.Error(), "unsupported signal 'SIGBOGUS'") || len(*kills) > 0 {
		t.Errorf("err = %v, kills = %v", err, *kills)
	}
}

func TestUninstallSignalWaitsForUpgradesUnlessAskedTo(t *testing.T) {
	ti, _, kills := signalling(t, `{"signal":["TERM","com.other"]}`)
	ti.in.upgrading = true
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if len(*kills) > 0 {
		t.Errorf("signalled during an upgrade: %v", *kills)
	}
	for _, onUpgrade := range []string{`"signal"`, `["signal"]`} {
		ti, _, kills := signalling(t, `{"signal":["TERM","com.other"],"on_upgrade":`+onUpgrade+`}`)
		ti.in.upgrading = true
		if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
			t.Fatal(err)
		}
		if len(*kills) != 1 {
			t.Errorf("on_upgrade %s: kills = %v", onUpgrade, *kills)
		}
	}
}

func TestUninstallSignalExpandsWildcards(t *testing.T) {
	ti, f, kills := signalling(t, `{"signal":["TERM","com.EXAMPLE.*"]}`)
	f.runningIDs = "com.example.app\norg.other.com.example.app\ncom.example.apple\n"
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := []string{fmt.Sprintf("123:%d", syscall.SIGTERM), fmt.Sprintf("456:%d", syscall.SIGTERM)}; !slices.Equal(*kills, want) {
		t.Errorf("kills = %v, want %v", *kills, want)
	}
}

func TestUninstallSignalWildcardNeedsAGUI(t *testing.T) {
	ti, f, kills := signalling(t, `{"signal":["TERM","com.example.*"]}`)
	f.gui = false
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: Not logged into a GUI; skipping applications matching 'com.example.*'.\n"; ti.warn.String() != want || len(*kills) > 0 {
		t.Errorf("warn = %q, kills = %v", ti.warn.String(), *kills)
	}
}

func TestUninstallKext(t *testing.T) {
	ti, _, r := withDirectives(t, `{"kext":"com.example.kext"}`)
	r.handle = func(c Command) ([]byte, error) {
		switch {
		case slices.Contains(c.Args, "/usr/sbin/kextstat"):
			return []byte("12 0 0xfff 0x1 com.example.kext (1.0)\n"), nil
		case slices.Contains(c.Args, "/usr/sbin/kextfind"):
			return []byte("/Library/Extensions/A.kext\n/Library/Extensions/B.kext\n"), nil
		}
		return nil, nil
	}
	var slept []string
	ti.in.Sleep = func(d time.Duration) { slept = append(slept, d.String()) }
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	root := func(name string, args ...string) Command {
		return Command{Name: "/usr/bin/sudo", Args: append([]string{"-u", "root", "-E", "--", name}, args...)}
	}
	want := []Command{
		root("/usr/sbin/kextstat", "-l", "-b", "com.example.kext"),
		root("/sbin/kextunload", "-b", "com.example.kext"),
		root("/usr/sbin/kextfind", "-b", "com.example.kext"),
		root("/bin/rm", "-rf", "/Library/Extensions/A.kext\n"),
		root("/bin/rm", "-rf", "/Library/Extensions/B.kext"),
	}
	if len(r.calls) != len(want) {
		t.Fatalf("calls = %+v", r.calls)
	}
	for i := range want {
		if r.calls[i].Name != want[i].Name || !slices.Equal(r.calls[i].Args, want[i].Args) {
			t.Errorf("call %d = %+v\nwant %+v", i, r.calls[i], want[i])
		}
	}
	if !slices.Equal(slept, []string{"1s"}) {
		t.Errorf("sleeps = %v", slept)
	}
	wantOut := "==> Unloading kernel extension com.example.kext\n==> Removing kernel extension /Library/Extensions/A.kext\n\n==> Removing kernel extension /Library/Extensions/B.kext\n"
	if !strings.Contains(ti.out.String(), wantOut) {
		t.Errorf("out = %q", ti.out.String())
	}
}

func TestUninstallKextThatIsNotLoaded(t *testing.T) {
	ti, _, r := withDirectives(t, `{"kext":["com.example.kext"]}`)
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != 2 || slices.Contains(r.calls[1].Args, "/sbin/kextunload") {
		t.Errorf("calls = %+v", r.calls)
	}
}

func TestUninstallKextRefusesToRunAsRoot(t *testing.T) {
	ti, _, r := withDirectives(t, `{"kext":"com.example.kext"}`)
	asRoot(t)
	err := ti.in.Uninstall(context.Background(), "app", "1.0", false)
	if err == nil || !strings.Contains(err.Error(), "Running Homebrew as root") || len(r.calls) > 0 {
		t.Errorf("err = %v, calls = %+v", err, r.calls)
	}
}

func TestUninstallDirectivesRunInBrewsOrder(t *testing.T) {
	ti, f, r := withDirectives(t, `{"delete":"/nonexistent/zzz","kext":"k","script":{"executable":"s.sh"},"early_script":{"executable":"e.sh"},"signal":["TERM","com.other"],"quit":"com.q","launchctl":"com.l"}`)
	stagedFile(t, ti, "s.sh")
	stagedFile(t, ti, "e.sh")
	var order []string
	f.handle = map[string]func([]string) ([]byte, error){
		"/bin/launchctl": func(args []string) ([]byte, error) {
			order = append(order, "launchctl "+args[0])
			if args[0] == "list" {
				return []byte("PID\tStatus\tLabel\n"), nil
			}
			return nil, errors.New("not found")
		},
		"osascript": func(args []string) ([]byte, error) {
			order = append(order, "osascript")
			return nil, errors.New("not running")
		},
	}
	r.handle = func(c Command) ([]byte, error) {
		order = append(order, filepath.Base(c.Args[len(c.Args)-1]))
		return nil, nil
	}
	_ = ti.in.Uninstall(context.Background(), "app", "1.0", false)
	var named []string
	for _, o := range order {
		if o == "e.sh" || o == "s.sh" || o == "k" {
			named = append(named, o)
		}
	}
	if want := []string{"e.sh", "k", "k", "s.sh"}; !slices.Equal(named, want) {
		t.Errorf("order = %v", order)
	}
}
