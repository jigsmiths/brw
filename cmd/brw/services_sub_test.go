package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServicesSubcommandAliases(t *testing.T) {
	for alias, want := range map[string]string{
		"launch": "Successfully started", "load": "Successfully started", "s": "Successfully started", "l": "Successfully started",
		"ls": "Name Status User File",
	} {
		e, prefix, _, _ := servicesEnv(t)
		args := []string{"services", alias, "-prefix", prefix}
		if alias != "ls" {
			args = append(args, "foo")
		}
		out, err := runCmd(t, e, args[0], args[1:]...)
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("%s: out %q, err %v, want %q", alias, out, err, want)
		}
	}
	for _, alias := range []string{"unload", "terminate", "term", "t", "u"} {
		e, prefix, _, lc := servicesEnv(t)
		lc.loaded["sh.brew.foo"] = runningOut
		out, err := runCmd(t, e, "services", alias, "-prefix", prefix, "foo")
		if err != nil || !strings.Contains(out, "Successfully stopped `foo`") {
			t.Errorf("%s: out %q, err %v", alias, out, err)
		}
	}
	for _, alias := range []string{"relaunch", "reload", "r"} {
		e, prefix, _, _ := servicesEnv(t)
		out, err := runCmd(t, e, "services", alias, "-prefix", prefix, "foo")
		if err != nil || !strings.Contains(out, "Successfully started `foo`") {
			t.Errorf("%s: out %q, err %v", alias, out, err)
		}
	}
}

func TestServicesUnknownSubcommand(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	_, err := runCmd(t, e, "services", "frob", "-prefix", prefix, "foo")
	if err == nil || err.Error() != "unknown subcommand: `frob`" {
		t.Errorf("err = %v", err)
	}
}

func TestServicesRejectsOptionsAndArgumentsASubcommandDoesNotTake(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list", "--all"}, "The `list` subcommand does not accept the `--all` switch."},
		{[]string{"list", "foo"}, "This command does not take named arguments."},
		{[]string{"start", "--json", "foo"}, "The `start` subcommand does not accept the `--json` switch."},
		{[]string{"stop", "--file", "x", "foo"}, "The `stop` subcommand does not accept the `--file` flag."},
		{[]string{"kill", "--keep", "foo"}, "The `kill` subcommand does not accept the `--keep` switch."},
		{[]string{"start", "--max-wait", "3", "foo"}, "The `start` subcommand does not accept the `--max-wait` flag."},
		{[]string{"cleanup", "--all"}, "The `cleanup` subcommand does not accept the `--all` switch."},
		{[]string{"cleanup", "foo"}, "This command does not take named arguments."},
		{[]string{"start", "--all", "--file", "x"}, "Options --all and --file are mutually exclusive."},
		{[]string{"stop", "--no-wait", "--max-wait", "3", "foo"}, "Options --max-wait and --no-wait are mutually exclusive."},
	} {
		e, prefix, _, lc := servicesEnv(t)
		_, err := runCmd(t, e, "services", append(tc.args, "-prefix", prefix)...)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: err = %v, want %q", tc.args, err, tc.want)
		}
		if len(lc.calls) != 0 {
			t.Errorf("%v ran %q", tc.args, lc.calls)
		}
	}
}

func TestServicesNeedAFormulaOrAll(t *testing.T) {
	for _, sub := range []string{"start", "stop", "restart", "run", "kill", "info"} {
		e, prefix, _, _ := servicesEnv(t)
		_, err := runCmd(t, e, "services", sub, "-prefix", prefix)
		if want := "Formula(e) missing, please provide a formula name or use `--all`."; err == nil || err.Error() != want {
			t.Errorf("%s: err = %v", sub, err)
		}
	}
}

func TestServicesAllOverridesNamedFormulae(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	out, err := runCmd(t, e, "services", "start", "--all", "-prefix", prefix, "plain")
	if err != nil || !strings.Contains(out, "Successfully started `foo`") {
		t.Errorf("out %q, err %v", out, err)
	}
	if want := "Warning: The `--all` argument overrides provided formula argument!\n"; stderr(e) != want {
		t.Errorf("stderr = %q, want %q", stderr(e), want)
	}
}

func TestServicesAllWithNothingToDoSucceeds(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "start", "--all", "-prefix", prefix)
	if err != nil || out != "" || len(lc.calls) != 0 {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
}

func TestServicesStopAllStopsOnlyLoadedServices(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	out, err := runCmd(t, e, "services", "stop", "--all", "-prefix", prefix)
	if err != nil || out != "" || len(lc.calls) != 0 {
		t.Errorf("nothing loaded: out %q, err %v, calls %q", out, err, lc.calls)
	}
	lc.loaded["sh.brew.foo"] = runningOut
	out, err = runCmd(t, e, "services", "stop", "--all", "-prefix", prefix)
	if err != nil || !strings.Contains(out, "Successfully stopped `foo` (label: sh.brew.foo)") {
		t.Errorf("out %q, err %v", out, err)
	}
}

func TestServicesRestartAll(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	lc.calls = nil
	if _, err := runCmd(t, e, "services", "restart", "--all", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	want := "bootout gui/501/sh.brew.foo\nenable gui/501/sh.brew.foo\nbootstrap gui/501 " + dst
	if calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
}

func TestServicesAllSkipsRootServicesWhenNotRoot(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	plist := filepath.Join(e.daemonsDir, "sh.brew.foo.plist")
	if err := os.WriteFile(plist, []byte(testPlist), 0o644); err != nil {
		t.Fatal(err)
	}
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "stop", "--all", "-prefix", prefix)
	if err != nil || out != "" || len(lc.calls) != 0 {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
}
