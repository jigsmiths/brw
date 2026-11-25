package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func plistAt(t *testing.T, path, body string) string {
	t.Helper()
	writeFile(t, path, body)
	return path
}

func TestServicesRunLoadsTheKegFileWithoutRegistering(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	out, err := runCmd(t, e, "services", "run", "-prefix", prefix, "foo")
	source := filepath.Join(prefix, "Cellar/foo/1.0/sh.brew.foo.plist")
	if want := "==> Successfully ran `foo` (label: sh.brew.foo)\n"; err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}
	if want := "bootstrap gui/501 " + source; calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
	if entries, _ := os.ReadDir(agents); len(entries) != 0 {
		t.Errorf("run registers nothing: %v", entries)
	}
}

func TestServicesRunReportsARunningService(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "run", "-prefix", prefix, "foo")
	want := "Service `foo` already running, use `brw services restart foo` to restart.\n"
	if err != nil || out != want || len(lc.calls) != 0 {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
}

func TestServicesRunCannotRunAsRoot(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	e.uid = 0
	out, err := runCmd(t, e, "services", "run", "-prefix", prefix, "foo")
	want := "Service `foo` cannot be run (but can be started) as root.\n"
	if err != nil || out != want || len(lc.calls) != 0 {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
}

func TestServicesRunAllRunsEveryService(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	out, err := runCmd(t, e, "services", "run", "--all", "-prefix", prefix)
	if want := "==> Successfully ran `foo` (label: sh.brew.foo)\n"; err != nil || out != want {
		t.Errorf("out %q, err %v", out, err)
	}
}

func TestServicesRunAndStartTakeAServiceFile(t *testing.T) {
	custom := "<plist><key>Label</key><string>custom.label</string></plist>\n"
	for _, verb := range []string{"run", "start", "restart"} {
		e, prefix, agents, lc := servicesEnv(t)
		file := plistAt(t, filepath.Join(t.TempDir(), "custom.plist"), custom)
		out, err := runCmd(t, e, "services", verb, "--file="+file, "-prefix", prefix, "foo")
		if err != nil || !strings.Contains(out, "(label: custom.label)") {
			t.Errorf("%s: out %q, err %v", verb, out, err)
		}
		if verb == "run" {
			if want := "bootstrap gui/501 " + file; calls(lc) != want {
				t.Errorf("run: calls = %q, want %q", calls(lc), want)
			}
			continue
		}
		dst := filepath.Join(agents, "sh.brew.foo.plist")
		if got, _ := os.ReadFile(dst); string(got) != custom {
			t.Errorf("%s: registered %q", verb, got)
		}
		if want := "enable gui/501/custom.label\nbootstrap gui/501 " + dst; calls(lc) != want {
			t.Errorf("%s: calls = %q, want %q", verb, calls(lc), want)
		}
	}
}

func TestServicesFileMustExist(t *testing.T) {
	for _, verb := range []string{"run", "start"} {
		e, prefix, _, lc := servicesEnv(t)
		_, err := runCmd(t, e, "services", verb, "--file", filepath.Join(t.TempDir(), "nope.plist"), "-prefix", prefix, "foo")
		if err == nil || err.Error() != "Provided service file does not exist." || len(lc.calls) != 0 {
			t.Errorf("%s: err %v, calls %q", verb, err, lc.calls)
		}
	}
}

func TestServicesStartAndRunMakeTheDirectoriesTheServiceNeeds(t *testing.T) {
	for _, verb := range []string{"start", "run"} {
		e, prefix, _, _ := servicesEnvFiles(t, file(".brew/foo.rb", infoFormula, 0o644), file("sh.brew.foo.plist", testPlist, 0o644))
		if err := os.RemoveAll(filepath.Join(prefix, "var")); err != nil {
			t.Fatal(err)
		}
		if _, err := runCmd(t, e, "services", verb, "-prefix", prefix, "foo"); err != nil {
			t.Fatal(err)
		}
		for _, dir := range []string{"var", "var/log"} {
			if fi, err := os.Stat(filepath.Join(prefix, dir)); err != nil || !fi.IsDir() {
				t.Errorf("%s: %s not made: %v", verb, dir, err)
			}
		}
	}
}

func TestServicesStartVerbosePrintsTheGeneratedFile(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	out, err := runCmd(t, e, "services", "start", "-v", "-prefix", prefix, "foo")
	want := "==> Generated service file for foo:\n   " + strings.ReplaceAll(testPlist, "\n", "\n   ") + "\n\n" +
		"==> Successfully started `foo` (label: sh.brew.foo)\n"
	if err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}
}

func TestServicesStopKeepLeavesTheServiceFile(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	dst := plistAt(t, filepath.Join(agents, "sh.brew.foo.plist"), testPlist)
	lc.loaded["sh.brew.foo"] = runningOut
	if _, err := runCmd(t, e, "services", "stop", "--keep", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Errorf("--keep removed the file: %v", err)
	}
	lc.loaded = map[string]string{}
	_, err := runCmd(t, e, "services", "stop", "--keep", "-prefix", prefix, "foo")
	if _, statErr := os.Stat(dst); statErr != nil {
		t.Errorf("--keep removed the file of a service that was not loaded: %v", statErr)
	}
	if err == nil || !strings.Contains(err.Error(), "is started as `me`") {
		t.Errorf("err = %v", err)
	}
}

func TestServicesStopNoWaitBootsOutOnceAndDoesNotSleep(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	lc.stubborn = 1000
	slept := 0
	e.sleep = func(time.Duration) { slept++ }
	out, err := runCmd(t, e, "services", "stop", "--no-wait", "-prefix", prefix, "foo")
	if err != nil || out != "Stopping `foo`...\n" || slept != 0 {
		t.Errorf("out %q, err %v, slept %d", out, err, slept)
	}
	want := "bootout gui/501/sh.brew.foo\nstop sh.brew.foo\nbootout user/501/sh.brew.foo\nstop sh.brew.foo"
	if calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
}

func TestServicesStopMaxWait(t *testing.T) {
	for _, tc := range []struct {
		maxWait string
		stuck   int
		want    int
	}{{"3", 1000, 6}, {"0", 5, 5}, {"2.5", 1000, 6}} {
		e, prefix, _, lc := servicesEnv(t)
		lc.loaded["sh.brew.foo"] = runningOut
		lc.stubborn = tc.stuck
		slept := 0
		e.sleep = func(time.Duration) { slept++ }
		if _, err := runCmd(t, e, "services", "stop", "--max-wait="+tc.maxWait, "-prefix", prefix, "foo"); err != nil {
			t.Fatal(err)
		}
		if slept != tc.want {
			t.Errorf("--max-wait=%s: slept %d, want %d", tc.maxWait, slept, tc.want)
		}
	}
}
