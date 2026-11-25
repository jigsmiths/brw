package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestServicesRefuseToRunUnderTmuxWithABrokenClipboard(t *testing.T) {
	t.Setenv("HOMEBREW_TMUX", "1")
	e, prefix, _, lc := servicesEnv(t)
	e.clipboardBroken = func() bool { return true }
	_, err := runCmd(t, e, "services", "list", "-prefix", prefix)
	if err == nil || err.Error() != "`brw services` cannot run under tmux!" || len(lc.calls) != 0 {
		t.Errorf("err = %v", err)
	}
	e.clipboardBroken = func() bool { return false }
	if _, err := runCmd(t, e, "services", "list", "-prefix", prefix); err != nil {
		t.Errorf("working clipboard: %v", err)
	}
	e.clipboardBroken = func() bool { return true }
	t.Setenv("HOMEBREW_TMUX", "")
	if _, err := runCmd(t, e, "services", "list", "-prefix", prefix); err == nil {
		t.Error("an empty HOMEBREW_TMUX counts as set in brew")
	}
	if err := os.Unsetenv("HOMEBREW_TMUX"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "services", "list", "-prefix", prefix); err != nil {
		t.Errorf("without HOMEBREW_TMUX: %v", err)
	}
}

func TestServicesDebugShowsTheLaunchctlQueries(t *testing.T) {
	for _, flag := range []string{"--debug", "-d"} {
		e, prefix, _, lc := servicesEnv(t)
		lc.loaded["sh.brew.foo"] = runningOut
		if _, err := runCmd(t, e, "services", "list", flag, "-prefix", prefix); err != nil {
			t.Fatal(err)
		}
		want := "==> /bin/launchctl print gui/501/sh.brew.foo\n" + runningOut + "==> /bin/launchctl list homebrew.mxcl.foo\n\n"
		if stderr(e) != want {
			t.Errorf("%s: stderr = %q, want %q", flag, stderr(e), want)
		}
	}
	e, prefix, _, _ := servicesEnv(t)
	if _, err := runCmd(t, e, "services", "list", "--debug", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	want := "==> /bin/launchctl list sh.brew.foo\n\n==> /bin/launchctl list homebrew.mxcl.foo\n\n"
	if stderr(e) != want {
		t.Errorf("not loaded: stderr = %q, want %q", stderr(e), want)
	}
}

func TestServicesDebugIsQuietWithoutTheFlag(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	if _, err := runCmd(t, e, "services", "list", "-prefix", prefix); err != nil || stderr(e) != "" {
		t.Errorf("stderr = %q, %v", stderr(e), err)
	}
}

func TestServicesQuietSystemCommandsAreNeverEchoed(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	if _, err := runCmd(t, e, "services", "kill", "-v", "-d", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr(e), "launchctl stop") {
		t.Errorf("quiet_system does not echo: %q", stderr(e))
	}
}

func TestServicesVerboseAndDebugEchoPlutil(t *testing.T) {
	for _, flags := range [][]string{{"-v", "-d"}, {"-v"}, {"-d"}} {
		e, prefix, agents, _ := servicesEnv(t)
		bin := plistAt(t, filepath.Join(agents, "sh.brew.bin.plist"), "bplist00x")
		e.plutil = func(string) ([]byte, error) { return []byte(plistFor("sh.brew.bin")), nil }
		if _, err := runCmd(t, e, "services", append([]string{"cleanup", "-prefix", prefix}, flags...)...); err != nil {
			t.Fatal(err)
		}
		want := "/usr/bin/plutil -convert xml1 -o - " + bin + "\n"
		if got := strings.Contains(stderr(e), want); got != (len(flags) == 2) {
			t.Errorf("%v: echoed %v, stderr %q", flags, got, stderr(e))
		}
	}
}

func TestServicesCleanupTreatsAMalformedPlistAsHavingNoLabel(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	bad := plistAt(t, filepath.Join(agents, "sh.brew.bad.plist"), "<plist><dict><key>Label</key><string>custom.bad</string></dict>")
	lc.loaded["custom.bad"] = "\tstate = not running\n\tlast exit code = 0\n"
	out, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	if want := "Removing unused service file: " + bad + "\n"; err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}
}

func TestServicesOhaiIsCutToTheTerminalWidthUnlessVerbose(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	e.tty = true
	e.termWidth = func() int { return 30 }
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	if want := "\x1b[34m==>\x1b[1m Successfully started `foo`\x1b[0m\n"; err != nil || out != want {
		t.Errorf("out %q, want %q, %v", out, want, err)
	}
	e, prefix, _, _ = servicesEnv(t)
	e.tty = true
	e.termWidth = func() int { return 30 }
	out, _ = runCmd(t, e, "services", "start", "-v", "-prefix", prefix, "foo")
	if !strings.Contains(out, "(label: sh.brew.foo)") {
		t.Errorf("verbose output is not cut: %q", out)
	}
}

func TestServicesColorFollowsTheEnvironmentLikeBrew(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	e.tty = true
	t.Setenv("NO_COLOR", "1")
	out, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	if want := "==> Successfully started `foo` (label: sh.brew.foo)\n"; err != nil || out != want {
		t.Errorf("NO_COLOR: out %q, want %q, %v", out, want, err)
	}
	t.Setenv("NO_COLOR", "")
	if err := os.Unsetenv("NO_COLOR"); err != nil {
		t.Fatal(err)
	}
	e, prefix, _, _ = servicesEnv(t)
	t.Setenv("HOMEBREW_COLOR", "1")
	out, _ = runCmd(t, e, "services", "start", "-prefix", prefix, "foo")
	if !strings.HasPrefix(out, "\x1b[34m==>") {
		t.Errorf("HOMEBREW_COLOR: out %q", out)
	}
}

func TestRunningLabelsMatchLikeBrew(t *testing.T) {
	jobs := "PID\tStatus\tLabel\n-\t0\thomebrew.mxcl\n42\t0\tsh.brew.foo.service\n-\t0\tcom.sh.brew.bar\n-\t0\tsh.brew.baz!\n-\t0\tcom.apple.x\n"
	want := []string{"homebrew.mxcl", "sh.brew.foo", "sh.brew.bar"}
	if got := runningLabels(jobs); !slices.Equal(got, want) {
		t.Errorf("labels = %q, want %q", got, want)
	}
}
