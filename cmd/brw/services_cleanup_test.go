package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func plistFor(label string) string {
	return "<plist><dict><key>Label</key><string>" + label + "</string></dict></plist>\n"
}

func TestServicesCleanupWithNothingToDo(t *testing.T) {
	for _, alias := range []string{"cleanup", "clean", "cl", "rm"} {
		e, prefix, _, lc := servicesEnv(t)
		out, err := runCmd(t, e, "services", alias, "-prefix", prefix)
		if want := "All user-space services OK, nothing cleaned...\n"; err != nil || out != want || len(lc.calls) != 0 {
			t.Errorf("%s: out %q, err %v, calls %q", alias, out, err, lc.calls)
		}
	}
	e, prefix, _, _ := servicesEnv(t)
	e.uid = 0
	out, _ := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	if want := "All root services OK, nothing cleaned...\n"; out != want {
		t.Errorf("as root: out %q, want %q", out, want)
	}
}

func TestServicesCleanupKillsRunningServicesWithoutAServiceFile(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	lc.loaded["homebrew.mxcl.ghost"] = runningOut
	lc.loaded["com.apple.thing"] = runningOut
	lc.stopKills = true
	out, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	want := "Killing `ghost`... (might take a while)\n==> Successfully killed `ghost` (label: homebrew.mxcl.ghost)\n" +
		"Killing `foo`... (might take a while)\n==> Successfully killed `foo` (label: sh.brew.foo)\n"
	if err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}
	if got := calls(lc); got != "stop homebrew.mxcl.ghost\nstop sh.brew.foo" {
		t.Errorf("calls = %q", got)
	}
	if entries, _ := os.ReadDir(agents); len(entries) != 0 {
		t.Errorf("wrote %v", entries)
	}
}

func TestServicesCleanupReportsALaunchctlThatCannotRun(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.jobsErr = errors.New("launchctl: not found")
	if _, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Errorf("err = %v", err)
	}
}

func TestServicesCleanupLeavesRegisteredServicesAlone(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	dst := plistAt(t, filepath.Join(agents, "sh.brew.foo.plist"), testPlist)
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	if want := "All user-space services OK, nothing cleaned...\n"; err != nil || out != want || len(lc.calls) != 0 {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
	if _, err := os.Stat(dst); err != nil {
		t.Error(err)
	}
}

func TestServicesCleanupSkipsServicesOfUnknownFormulae(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["homebrew.mxcl.nosuchthing"] = runningOut
	out, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	if want := "All user-space services OK, nothing cleaned...\n"; err != nil || out != want {
		t.Errorf("out %q, err %v", out, err)
	}
	if want := "Warning: Service homebrew.mxcl.nosuchthing not managed by `brw services` => skipping\n"; stderr(e) != want {
		t.Errorf("stderr = %q, want %q", stderr(e), want)
	}
	if len(lc.calls) != 0 {
		t.Errorf("calls = %q", lc.calls)
	}
}

func TestServicesCleanupRemovesUnusedServiceFiles(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	write := func(name, body string) string { return plistAt(t, filepath.Join(agents, name), body) }
	stale := write("sh.brew.stale.plist", plistFor("sh.brew.stale"))
	staleTimer := write("homebrew.mxcl.stale.timer", "x")
	staleUnit := write("sh.brew.stale.service", "x")
	loaded := write("sh.brew.foo.plist", testPlist)
	relabeled := write("sh.brew.other.plist", plistFor("custom.label"))
	foreign := write("com.example.thing.plist", plistFor("com.example.thing"))
	lc.loaded["sh.brew.foo"] = "\tstate = not running\n\tlast exit code = 0\n"
	lc.loaded["custom.label"] = "\tstate = not running\n\tlast exit code = 0\n"

	out, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	want := "Removing unused service file: " + staleTimer + "\n" +
		"Removing unused service file: " + stale + "\n" +
		"Removing unused service file: " + staleUnit + "\n"
	if err != nil || out != want {
		t.Errorf("out:\n%s\nwant:\n%s\n%v", out, want, err)
	}
	for _, p := range []string{stale, staleTimer, staleUnit} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s still there: %v", p, err)
		}
	}
	for _, p := range []string{loaded, relabeled, foreign} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("%s was removed: %v", p, err)
		}
	}
}

func TestServicesCleanupFailsOnADirectoryThatMatchesTheGlob(t *testing.T) {
	e, prefix, agents, _ := servicesEnv(t)
	dir := filepath.Join(agents, "sh.brew.dir.plist")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	if want := "Removing unused service file: " + dir + "\n"; out != want {
		t.Errorf("out %q, want %q", out, want)
	}
	if want := "Operation not permitted @ apply2files - " + dir; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory removed: %v", err)
	}
}

func TestServicesCleanupReadsTheLabelOfABinaryPlistWithPlutil(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	kept := plistAt(t, filepath.Join(agents, "sh.brew.kept.plist"), "bplist00kept")
	gone := plistAt(t, filepath.Join(agents, "sh.brew.gone.plist"), "bplist00gone")
	e.plutil = func(path string) ([]byte, error) {
		return []byte(plistFor("custom." + strings.TrimSuffix(strings.TrimPrefix(filepath.Base(path), "sh.brew."), ".plist"))), nil
	}
	lc.loaded["custom.kept"] = "\tstate = not running\n\tlast exit code = 0\n"
	out, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	if want := "Removing unused service file: " + gone + "\n"; err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}
	if _, err := os.Stat(kept); err != nil {
		t.Errorf("a loaded service's file was removed: %v", err)
	}
}

func TestServicesCleanupUsesTheDaemonsDirectoryAsRoot(t *testing.T) {
	e, prefix, agents, _ := servicesEnv(t)
	user := plistAt(t, filepath.Join(agents, "sh.brew.stale.plist"), plistFor("sh.brew.stale"))
	system := plistAt(t, filepath.Join(e.daemonsDir, "sh.brew.stale.plist"), plistFor("sh.brew.stale"))
	e.uid = 0
	out, err := runCmd(t, e, "services", "cleanup", "-prefix", prefix)
	if want := "Removing unused service file: " + system + "\n"; err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}
	if _, err := os.Stat(user); err != nil {
		t.Errorf("root cleanup touched the user directory: %v", err)
	}
}

func TestServicesCleanupRecordsRemovalsInTheJournal(t *testing.T) {
	e, prefix, agents, _ := servicesEnv(t)
	plistAt(t, filepath.Join(agents, "sh.brew.stale.plist"), plistFor("sh.brew.stale"))
	journal := filepath.Join(t.TempDir(), "journal")
	if _, err := runCmd(t, e, "services", "cleanup", "-journal", journal, "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(journal); !strings.Contains(string(got), "sh.brew.stale.plist") {
		t.Errorf("journal = %q", got)
	}
}
