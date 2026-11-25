package main

import (
	"os"
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

func TestServicesVerboseAndDebugEchoTheQuietLaunchctlCommands(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	if _, err := runCmd(t, e, "services", "kill", "-v", "-d", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr(e), "/bin/launchctl stop sh.brew.foo\n") {
		t.Errorf("stderr = %q", stderr(e))
	}
	e, prefix, _, lc = servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	if _, err := runCmd(t, e, "services", "kill", "-v", "-prefix", prefix, "foo"); err != nil || strings.Contains(stderr(e), "/bin/launchctl") {
		t.Errorf("-v alone does not echo quiet commands: %q, %v", stderr(e), err)
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

func TestRunningLabelsMatchLikeBrew(t *testing.T) {
	jobs := "PID\tStatus\tLabel\n-\t0\thomebrew.mxcl\n42\t0\tsh.brew.foo.service\n-\t0\tcom.sh.brew.bar\n-\t0\tsh.brew.baz!\n-\t0\tcom.apple.x\n"
	want := []string{"homebrew.mxcl", "sh.brew.foo", "sh.brew.bar"}
	if got := runningLabels(jobs); !slices.Equal(got, want) {
		t.Errorf("labels = %q, want %q", got, want)
	}
}
