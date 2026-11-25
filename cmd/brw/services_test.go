package main

import (
	"errors"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/services"
)

const (
	testPlist   = "<plist><key>Label</key><string>sh.brew.foo</string></plist>\n"
	legacyPlist = "<plist><key>Label</key><string>homebrew.mxcl.foo</string></plist>\n"
	runningOut  = "\tstate = running\n\tpid = 42\n"
)

type fakeLaunchctl struct {
	calls     []string
	loaded    map[string]string
	stubborn  int
	bootouts  int
	bootoutOK bool
	stopKills bool
}

func (f *fakeLaunchctl) Jobs() string {
	var b strings.Builder
	b.WriteString("PID\tStatus\tLabel\n")
	for _, label := range slices.Sorted(maps.Keys(f.loaded)) {
		pid := "-"
		if p := services.ParsePrint(f.loaded[label]); p.Running() {
			pid = strconv.Itoa(p.PID)
		}
		b.WriteString(pid + "\t0\t" + label + "\n")
	}
	return b.String()
}

func (f *fakeLaunchctl) Print(domain, label string) (string, error) {
	out, ok := f.loaded[label]
	if !ok {
		return "", services.ErrNotLoaded
	}
	return out, nil
}

func (f *fakeLaunchctl) List(label string) (string, error) { return "", services.ErrNotLoaded }

func (f *fakeLaunchctl) Enable(domain, label string) error {
	f.calls = append(f.calls, "enable "+domain+"/"+label)
	return nil
}

func (f *fakeLaunchctl) Bootstrap(domain, file string) error {
	f.calls = append(f.calls, "bootstrap "+domain+" "+file)
	f.loaded[strings.TrimSuffix(filepath.Base(file), ".plist")] = runningOut
	return nil
}

func (f *fakeLaunchctl) Bootout(domain, label string) error {
	f.calls = append(f.calls, "bootout "+domain+"/"+label)
	f.bootouts++
	if _, ok := f.loaded[label]; !ok && !f.bootoutOK {
		return errors.New("not loaded")
	}
	if f.bootouts > f.stubborn {
		delete(f.loaded, label)
	}
	return nil
}

func (f *fakeLaunchctl) Stop(label string) error {
	f.calls = append(f.calls, "stop "+label)
	if _, ok := f.loaded[label]; ok && f.stopKills {
		f.loaded[label] = "\tstate = not running\n\tlast exit code = 0\n"
	}
	return nil
}

func servicesEnvWith(t *testing.T, plistName, body string) (*env, string, string, *fakeLaunchctl) {
	t.Helper()
	return servicesEnvFiles(t, file(plistName, body, 0o644), file(".brew/foo.rb", blockOnlyFormula, 0o644))
}

const blockOnlyFormula = `class Foo < Formula
  url "https://example.com/foo-1.0.tar.gz"
  sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

  service do
    run_at_load true
  end
end
`

func TestServicesListsOnlyFormulaeWithAServiceBlock(t *testing.T) {
	e, prefix, _, _ := servicesEnvFiles(t, file("sh.brew.foo.plist", testPlist, 0o644))
	out, err := runCmd(t, e, "services", "list", "-prefix", prefix)
	if err != nil || out != "" {
		t.Errorf("list: out %q, err %v", out, err)
	}
	for _, sub := range []string{"start", "stop", "run", "kill", "info"} {
		out, err := runCmd(t, e, "services", sub, "--all", "-prefix", prefix)
		if err != nil || out != "" {
			t.Errorf("%s --all: out %q, err %v", sub, out, err)
		}
	}
}

func servicesEnvFiles(t *testing.T, extra ...installtest.File) (*env, string, string, *fakeLaunchctl) {
	t.Helper()
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "foo", Version: "1.0", Files: append([]installtest.File{file("bin/foo", "foo", 0o755)}, extra...)}, nil, nil)
	s.formula(installtest.Bottle{Name: "plain", Version: "1.0", Files: []installtest.File{file("bin/plain", "x", 0o755)}}, nil, nil)
	s.formula(installtest.Bottle{Name: "ghost", Version: "1.0", Files: []installtest.File{file("bin/ghost", "x", 0o755)}}, nil, nil)
	s.formula(installtest.Bottle{Name: "daemon", Version: "1.0", Files: []installtest.File{file("bin/daemon", "x", 0o755)}}, nil, map[string]any{
		"service": map[string]any{
			"run": []string{"$HOMEBREW_PREFIX/opt/daemon/bin/daemon", "serve"}, "run_type": "immediate",
			"keep_alive": map[string]any{"always": true}, "working_dir": "$HOMEBREW_PREFIX/var",
		},
	})
	e, prefix := s.env()
	for _, n := range []string{"foo", "plain"} {
		if _, err := runCmd(t, e, "install", "-prefix", prefix, n); err != nil {
			t.Fatal(err)
		}
	}
	lc := &fakeLaunchctl{loaded: map[string]string{}}
	agents := t.TempDir()
	e.launchctl, e.agentsDir, e.daemonsDir, e.uid, e.user = lc, agents, t.TempDir(), 501, "me"
	e.configHome = t.TempDir()
	e.sleep = func(time.Duration) {}
	e.err.(interface{ Reset() }).Reset()
	return e, prefix, agents, lc
}

func servicesEnv(t *testing.T) (*env, string, string, *fakeLaunchctl) {
	return servicesEnvWith(t, "sh.brew.foo.plist", testPlist)
}

func calls(lc *fakeLaunchctl) string { return strings.Join(lc.calls, "\n") }

func TestServicesStart(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	got, err := os.ReadFile(dst)
	if err != nil || string(got) != testPlist {
		t.Fatalf("copied plist = %q, %v", got, err)
	}
	if fi, _ := os.Stat(dst); fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if want := "enable gui/501/sh.brew.foo\nbootstrap gui/501 " + dst; calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
	if want := "==> Successfully started `foo` (label: sh.brew.foo)\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestServicesStartLegacyPlistKeepsBrewsNames(t *testing.T) {
	e, prefix, agents, lc := servicesEnvWith(t, "homebrew.mxcl.foo.plist", legacyPlist)
	stale := filepath.Join(agents, "homebrew.mxcl.foo.plist")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("start leaves the old copy of the service: %v", err)
		}
	}()
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("brew copies to the canonical name: %v", err)
	}
	if want := "enable gui/501/homebrew.mxcl.foo\nbootstrap gui/501 " + dst; calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
	if !strings.Contains(out, "(label: homebrew.mxcl.foo)") {
		t.Errorf("out = %q", out)
	}
}

func TestServicesStartAlreadyStarted(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	want := "Service `foo` already started, use `brw services restart foo` to restart.\n"
	if err != nil || out != want {
		t.Errorf("out = %q, %v", out, err)
	}
	if len(lc.calls) != 0 {
		t.Errorf("calls = %q", lc.calls)
	}
	if entries, _ := os.ReadDir(agents); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestServicesStartLoadedUnderAnotherLabel(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["homebrew.mxcl.foo"] = runningOut
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	want := "Service `foo` is already loaded as `homebrew.mxcl.foo`; a service label migration is pending. " +
		"Use `brw services restart foo` to migrate to `sh.brew.foo`.\n"
	if err != nil || out != want || len(lc.calls) != 0 {
		t.Errorf("out = %q, %v, calls %q", out, err, lc.calls)
	}
}

func TestServicesStartLoadedButNotRunningStillStarts(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = "\tstate = not running\n"
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if len(lc.calls) != 2 {
		t.Errorf("brew only skips a running service: %q", lc.calls)
	}
}

func TestServicesMessagesAreColoredOnATerminal(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	e.tty = true
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	want := "\x1b[34m==>\x1b[1m Successfully started `foo` (label: sh.brew.foo)\x1b[0m\n"
	if err != nil || out != want {
		t.Errorf("out = %q, want %q, %v", out, want, err)
	}
	if _, err := runCmd(t, e, "services", "stop", "-prefix", prefix, "plain"); err != nil || !strings.HasPrefix(stderr(e), "\x1b[33mWarning\x1b[0m: ") {
		t.Errorf("stderr = %q, %v", stderr(e), err)
	}
}

func TestInstalledServiceSaysWhetherThereIsOne(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	kegs, err := e.kegs(prefix)
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := e.installedService(prefix, kegs, "foo"); !ok || !got.hasService || got.source == "" {
		t.Errorf("foo = %+v, %v", got, ok)
	}
	if got, ok := e.installedService(prefix, kegs, "plain"); ok || got.hasService {
		t.Errorf("plain = %+v, %v", got, ok)
	}
	if got, ok := e.installedService(prefix, kegs, "ghost"); ok || got.hasService || got.installed {
		t.Errorf("ghost = %+v, %v", got, ok)
	}
}

func TestServicesStop(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	lc.calls = nil
	out, err := runCmd(t, e, "services", "stop", "-prefix", prefix, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if calls(lc) != "bootout gui/501/sh.brew.foo" {
		t.Errorf("calls = %q", lc.calls)
	}
	if _, err := os.Stat(filepath.Join(agents, "sh.brew.foo.plist")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still there: %v", err)
	}
	want := "Stopping `foo`... (might take a while)\n==> Successfully stopped `foo` (label: sh.brew.foo)\n"
	if out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestServicesStopWaitsUntilUnloaded(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	lc.stubborn = 2
	var slept []time.Duration
	e.sleep = func(d time.Duration) { slept = append(slept, d) }
	if _, err := runCmd(t, e, "services", "stop", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 2 || slept[0] != time.Second {
		t.Errorf("sleeps = %v", slept)
	}
	if lc.bootouts != 3 {
		t.Errorf("bootouts = %d", lc.bootouts)
	}
}

func TestServicesStopGivesUpAfterAMinute(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	if err := os.WriteFile(filepath.Join(agents, "sh.brew.foo.plist"), []byte(testPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	lc.loaded["sh.brew.foo"] = runningOut
	lc.stubborn = 1000
	var slept int
	e.sleep = func(time.Duration) { slept++ }
	if _, err := runCmd(t, e, "services", "stop", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr(e), "Warning: Unable to stop `foo` (label: sh.brew.foo)") {
		t.Errorf("stderr = %q", stderr(e))
	}
	if !strings.Contains(calls(lc), "stop sh.brew.foo") || slept == 0 {
		t.Errorf("calls = %q, slept %d", lc.calls, slept)
	}
	if _, err := os.Stat(filepath.Join(agents, "sh.brew.foo.plist")); err != nil {
		t.Errorf("a service that would not stop keeps its plist: %v", err)
	}
}

func TestServicesStopNotStarted(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	out, err := runCmd(t, e, "services", "stop", "-prefix", prefix, "foo")
	if err != nil || out != "" || stderr(e) != "Warning: Service `foo` is not started.\n" {
		t.Errorf("out %q, err %v, stderr %q", out, err, stderr(e))
	}
	if want := "bootout gui/501/sh.brew.foo\nbootout gui/501/homebrew.mxcl.foo"; calls(lc) != want {
		t.Errorf("calls = %q", lc.calls)
	}
}

func TestServicesRestart(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	lc.calls = nil
	if _, err := runCmd(t, e, "services", "restart", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	want := "bootout gui/501/sh.brew.foo\nenable gui/501/sh.brew.foo\nbootstrap gui/501 " + dst
	if calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Error(err)
	}
}

func TestServicesRestartRunsAServiceBrewDidNotRegister(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "restart", "-prefix", prefix, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "==> Successfully ran `foo` (label: sh.brew.foo)") {
		t.Errorf("out = %q", out)
	}
	if strings.Contains(calls(lc), "enable") {
		t.Errorf("run does not enable: %q", lc.calls)
	}
	if entries, _ := os.ReadDir(agents); len(entries) != 0 {
		t.Errorf("run registers nothing: %v", entries)
	}
}

func TestServicesDryRun(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	out, err := runCmd(t, e, "services", "restart", "-n", "-prefix", prefix, "foo")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	for _, want := range []string{"launchctl bootout gui/501/sh.brew.foo", "cp ", dst, "launchctl enable gui/501/sh.brew.foo", "launchctl bootstrap gui/501 " + dst} {
		if !strings.Contains(out, want) {
			t.Errorf("dry run output %q lacks %q", out, want)
		}
	}
	if len(lc.calls) != 0 {
		t.Errorf("dry run ran %q", lc.calls)
	}
	if entries, _ := os.ReadDir(agents); len(entries) != 0 {
		t.Errorf("dry run wrote %v", entries)
	}
}

func TestServicesErrors(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	for name, want := range map[string]string{
		"nosuchthing": "no available formula",
		"plain":       "formula `plain` has not implemented #plist, #service or provided a locatable service file",
		"ghost":       "formula `ghost` is not installed",
	} {
		_, err := runCmd(t, e, "services", "start", "-prefix", prefix, name)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("start %s: %v, want %q", name, err, want)
		}
	}
	if len(lc.calls) != 0 {
		t.Errorf("errors ran %q", lc.calls)
	}
	e.host = platform.Host{OS: "linux", Arch: "amd64"}
	_, err := runCmd(t, e, "services", "list", "-prefix", prefix)
	if err == nil || !strings.Contains(err.Error(), "unlike brew, brw services does not support systemd on Linux yet") {
		t.Errorf("on linux: %v", err)
	}
}

func TestServicesListShowsInstalledFormulaeWithServicesOnly(t *testing.T) {
	e, prefix, agents, _ := servicesEnv(t)
	if err := os.WriteFile(filepath.Join(agents, "sh.atuin.daemon.plist"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agents, "homebrew.mxcl.stale.plist"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "services", "-prefix", prefix)
	want := "Name Status User File\n" + "foo  " + "none" + strings.Repeat(" ", 11) + " " + strings.Repeat(" ", 4) + " \n"
	if err != nil || out != want {
		t.Errorf("out = %q, want %q, %v", out, want, err)
	}
}

func TestServicesListStatuses(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	if err := os.WriteFile(dst, []byte(testPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	row := func(status, file string) string {
		return "foo  " + status + strings.Repeat(" ", 15-len(status)) + " me   " + file + "\n"
	}

	out, _ := runCmd(t, e, "services", "list", "-prefix", prefix)
	if want := "Name Status User File\n" + row("none", ""); out != want {
		t.Errorf("registered but not loaded: %q, want %q", out, want)
	}

	lc.loaded["sh.brew.foo"] = runningOut
	out, _ = runCmd(t, e, "services", "list", "-prefix", prefix)
	if want := "Name Status User File\n" + row("started", dst); out != want {
		t.Errorf("started: %q, want %q", out, want)
	}

	real, err := os.ReadFile("../../internal/services/testdata/print_rift.txt")
	if err != nil {
		t.Fatal(err)
	}
	lc.loaded["sh.brew.foo"] = string(real)
	out, _ = runCmd(t, e, "services", "list", "-prefix", prefix)
	if want := "Name Status User File\n" + row("error  78", dst); out != want {
		t.Errorf("error: %q, want %q", out, want)
	}

	lc.loaded["sh.brew.foo"] = "\tstate = not running\n\tlast exit code = 0\n"
	out, _ = runCmd(t, e, "services", "list", "-prefix", prefix)
	if want := "Name Status User File\n" + row("stopped", dst); out != want {
		t.Errorf("stopped: %q, want %q", out, want)
	}

	lc.loaded["sh.brew.foo"] = "\tstate = not running\n"
	out, _ = runCmd(t, e, "services", "list", "-prefix", prefix)
	if want := "Name Status User File\n" + row("other", dst); out != want {
		t.Errorf("other: %q, want %q", out, want)
	}
}

func TestServicesListJSON(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	out, err := runCmd(t, e, "services", "list", "-json", "-prefix", prefix)
	opt := filepath.Join(prefix, "opt/foo/sh.brew.foo.plist")
	want := "[\n  {\n    \"name\": \"foo\",\n    \"status\": \"none\",\n    \"user\": null,\n    \"file\": \"" + opt + "\",\n    \"exit_code\": null\n  }\n]\n"
	if err != nil || out != want {
		t.Errorf("unregistered:\n%s\nwant\n%s (%v)", out, want, err)
	}

	dst := filepath.Join(agents, "sh.brew.foo.plist")
	if err := os.WriteFile(dst, []byte(testPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	lc.loaded["sh.brew.foo"] = "\tstate = not running\n\tlast exit code = 78: EX_CONFIG\n"
	out, _ = runCmd(t, e, "services", "list", "-json", "-prefix", prefix)
	want = "[\n  {\n    \"name\": \"foo\",\n    \"status\": \"error\",\n    \"user\": \"me\",\n    \"file\": \"" + dst + "\",\n    \"exit_code\": 78\n  }\n]\n"
	if out != want {
		t.Errorf("registered:\n%s\nwant\n%s", out, want)
	}
}

func TestServicesListJSONWithNoServices(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	if err := os.RemoveAll(filepath.Join(prefix, "Cellar/foo")); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "services", "list", "--json", "-prefix", prefix)
	if err != nil || out != "[]\n" {
		t.Errorf("out = %q, %v", out, err)
	}
}

const envPlist = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\">\n<dict>\n" +
	"\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>PATH</key>\n\t\t<string>/bin</string>\n\t</dict>\n" +
	"\t<key>Label</key>\n\t<string>sh.brew.foo</string>\n</dict>\n</plist>\n"

func writeEnvFile(t *testing.T, e *env, mode os.FileMode, body string) string {
	t.Helper()
	dir := filepath.Join(e.configHome, "services")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "foo.env")
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

const envFormula = `class Foo < Formula
  desc "Foo"
  homepage "https://example.com"
  url "https://example.com/foo-1.0.tar.gz"
  sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

  service do
    run [opt_bin/"foo", "serve"]
    environment_variables PATH: "/bin"
  end
end
`

func envServicesEnv(t *testing.T) (*env, string, string, *fakeLaunchctl) {
	t.Helper()
	return servicesEnvFiles(t, file(".brew/foo.rb", envFormula, 0o644), file("sh.brew.foo.plist", envPlist, 0o644))
}

func TestServicesStartAppliesTheUserEnvFile(t *testing.T) {
	e, prefix, agents, _ := envServicesEnv(t)
	writeEnvFile(t, e, 0o600, "# mine\nPATH=/x:/bin\nFOO=bar & baz\n")
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(agents, "sh.brew.foo.plist"))
	want := "\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>FOO</key>\n\t\t<string>bar &amp; baz</string>\n\t\t<key>PATH</key>\n\t\t<string>/x:/bin</string>\n\t</dict>\n"
	if !strings.Contains(string(got), want) {
		t.Errorf("plist:\n%s\nwant it to contain\n%s", got, want)
	}
	if stderr(e) != "" {
		t.Errorf("stderr = %q", stderr(e))
	}
}

func TestServicesStartLeavesAShippedPlistAlone(t *testing.T) {
	e, prefix, agents, _ := servicesEnvWith(t, "sh.brew.foo.plist", envPlist)
	writeEnvFile(t, e, 0o600, "FOO=bar\n")
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(agents, "sh.brew.foo.plist"))
	if string(got) != envPlist || stderr(e) != "" {
		t.Errorf("a plist the formula ships is copied as it is: %q, stderr %q", got, stderr(e))
	}
}

func TestServicesStartSkipsAnUnsafeEnvFile(t *testing.T) {
	for mode, kind := range map[os.FileMode]string{0o666: "world-writable", 0o664: "group-writable"} {
		e, prefix, agents, _ := envServicesEnv(t)
		path := writeEnvFile(t, e, mode, "FOO=bar\n")
		if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(filepath.Join(agents, "sh.brew.foo.plist"))
		if strings.Contains(string(got), "<key>FOO</key>") {
			t.Errorf("%s: env was applied:\n%s", kind, got)
		}
		if want := "Warning: Skipping " + path + ": file is " + kind + ".\n"; stderr(e) != want {
			t.Errorf("%s: stderr = %q, want %q", kind, stderr(e), want)
		}
	}
}

func TestServicesStartWarnsAboutInvalidEnvLines(t *testing.T) {
	e, prefix, agents, _ := envServicesEnv(t)
	path := writeEnvFile(t, e, 0o600, "nokey\nFOO=1\n")
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: Skipping invalid line in " + path + ": nokey\n"; stderr(e) != want {
		t.Errorf("stderr = %q, want %q", stderr(e), want)
	}
	if got, _ := os.ReadFile(filepath.Join(agents, "sh.brew.foo.plist")); !strings.Contains(string(got), "<key>FOO</key>") {
		t.Errorf("valid lines still apply:\n%s", got)
	}
}

const explicitNameFormula = `class Foo < Formula
  desc "Foo"
  homepage "https://example.com"
  url "https://example.com/foo-1.0.tar.gz"
  sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

  service do
    run [opt_bin/"foo", "serve"]
    name macos: "custom.foo"
    environment_variables FROM_FORMULA: "1"
  end
end
`

func TestServicesRegenerateThePlistFromTheFormulaWithItsExplicitName(t *testing.T) {
	e, prefix, agents, lc := servicesEnvFiles(t,
		file("custom.foo.plist", "<stale>", 0o644), file(".brew/foo.rb", explicitNameFormula, 0o644))
	writeEnvFile(t, e, 0o600, "FOO=bar\n")
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(agents, "custom.foo.plist")
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("explicit plist_name decides the file: %v", err)
	}
	for _, want := range []string{"<string>custom.foo</string>", "<string>" + prefix + "/opt/foo/bin/foo</string>", "<key>FROM_FORMULA</key>", "<key>FOO</key>"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("regenerated plist lacks %q:\n%s", want, got)
		}
	}
	if want := "enable gui/501/custom.foo\nbootstrap gui/501 " + dst; calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
	lc.calls = nil
	lc.loaded = map[string]string{"sh.brew.foo": runningOut}
	if out, _ := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); strings.Contains(out, "already") || len(lc.calls) == 0 {
		t.Errorf("an explicit name is not also probed under brew's defaults: %q %q", out, lc.calls)
	}
}

func TestServicesRunLoadsAnEditedCopyWhenTheEnvFileChangesIt(t *testing.T) {
	e, prefix, agents, lc := envServicesEnv(t)
	writeEnvFile(t, e, 0o600, "FOO=bar\n")
	lc.loaded["sh.brew.foo"] = runningOut
	if _, err := runCmd(t, e, "services", "restart", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(prefix, "Cellar/foo/1.0/sh.brew.foo.plist")
	var boot string
	for _, c := range lc.calls {
		if strings.HasPrefix(c, "bootstrap ") {
			boot = strings.TrimPrefix(c, "bootstrap gui/501 ")
		}
	}
	if boot == "" || boot == source {
		t.Errorf("run must load a copy with the env applied, not the keg file: %q", lc.calls)
	}
	if entries, _ := os.ReadDir(agents); len(entries) != 0 {
		t.Errorf("run registers nothing: %v", entries)
	}
}
