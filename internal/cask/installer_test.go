package cask

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
)

func TestPlanReadsInstallerStanzas(t *testing.T) {
	c := api.Cask{Token: "a", Artifacts: raws(t, `[
		{"installer":[{"script":"setup.sh"}]},
		{"installer":[{"script":{"executable":"i.sh","args":["--quiet",1],"input":["y\n","n\n"],"must_succeed":false,"sudo":true,"print_stdout":false,"bogus":1}}]},
		{"installer":[{"manual":"Setup.app"}]}]`)}
	p, err := plan(c, where)
	if err != nil {
		t.Fatal(err)
	}
	want := []installerStep{
		{path: "setup.sh", mustSucceed: true, printStdout: true, printStderr: true},
		{path: "i.sh", args: []string{"--quiet", "1"}, input: "y\nn\n", sudo: true, printStderr: true, ignored: []string{"bogus"}},
		{manual: true, path: "Setup.app"},
	}
	if !reflect.DeepEqual(p.installers, want) {
		t.Errorf("installers = %+v, want %+v", p.installers, want)
	}
	if got := len(p.uninstall); got != 3 {
		t.Errorf("receipt entries = %d, want 3", got)
	}
}

func TestPlanRejectsBadInstallerStanzas(t *testing.T) {
	for stanza, want := range map[string]string{
		`{"installer":[{"script":{"args":["x"]}}]}`: "Cask 'a' definition is invalid: installer missing executable",
		`{"installer":[{"manual":3}]}`:              "Cask 'a' definition is invalid: installer requires a path for :manual",
	} {
		c := api.Cask{Token: "a", Artifacts: raws(t, `[`+stanza+`,{"app":["A.app"]}]`)}
		if _, err := plan(c, where); err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", stanza, err, want)
		}
	}
}

type fakeScripts struct {
	runs []ScriptRun
	err  error
	seen func(ScriptRun)
}

func (f *fakeScripts) run(_ context.Context, r ScriptRun) error {
	f.runs = append(f.runs, r)
	if f.seen != nil {
		f.seen(r)
	}
	return f.err
}

var installerTarball = func() []byte {
	var t testing.T
	return tarGzBytes(&t,
		entry{name: "App.app/Contents/Info.plist", body: "<plist/>"},
		entry{name: "setup.sh", body: "#!/bin/sh\n", mode: 0o644})
}()

func TestInstallRunsInstallerScriptBeforeApps(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	ti := newTestInstall(t, installerTarball)
	f := &fakeScripts{}
	ti.in.Script = f.run
	f.seen = func(ScriptRun) {
		if exists(filepath.Join(ti.appDir, "App.app")) {
			t.Error("the installer ran after the app was moved")
		}
	}
	c := ti.cask(t, `[
		{"installer":[{"script":{"executable":"setup.sh","args":["--quiet"],"input":"yes\n"}}]},
		{"app":["App.app"]}]`)
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/setup.sh")
	want := ScriptRun{
		Executable: staged, Args: []string{"--quiet"}, Input: "yes\n", MustSucceed: true,
		PrintStdout: true, PrintStderr: true,
		Path: ti.prefix + "/bin:" + ti.prefix + "/sbin:/usr/bin:/bin",
	}
	if len(f.runs) == 1 {
		f.runs[0].Stdout, f.runs[0].Stderr = nil, nil
	}
	if len(f.runs) != 1 || !reflect.DeepEqual(f.runs[0], want) {
		t.Fatalf("runs = %+v, want %+v", f.runs, want)
	}
	if !strings.Contains(ti.out.String(), "==> Running installer script 'setup.sh'\n") {
		t.Errorf("output = %q", ti.out.String())
	}
	if fi, err := os.Stat(staged); err != nil || fi.Mode().Perm()&0o111 != 0o111 {
		t.Errorf("staged script not made executable: %v %v", fi, err)
	}
}

func TestInstallerScriptFailureRollsBack(t *testing.T) {
	ti := newTestInstall(t, installerTarball)
	ti.in.Script = (&fakeScripts{err: errors.New("boom")}).run
	c := ti.cask(t, `[{"installer":[{"script":"setup.sh"}]},{"app":["App.app"]}]`)
	if err := ti.in.Install(context.Background(), c); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err = %v, want boom", err)
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) || exists(filepath.Join(ti.appDir, "App.app")) {
		t.Error("a failed install left files behind")
	}
}

func TestInstallManualInstallerPrintsHowToOpen(t *testing.T) {
	ti := newTestInstall(t, installerTarball)
	f := &fakeScripts{}
	ti.in.Script = f.run
	c := ti.cask(t, `[{"installer":[{"manual":"My Setup.app"}]},{"app":["App.app"]}]`)
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	want := "Cask app only provides a manual installer. To run it and complete the installation:\n  open " +
		strings.ReplaceAll(filepath.Join(ti.prefix, "Caskroom/app/1.0/My Setup.app"), " ", `\ `) + "\n"
	if !strings.Contains(ti.out.String(), want) {
		t.Errorf("output = %q, want it to contain %q", ti.out.String(), want)
	}
	if len(f.runs) != 0 {
		t.Errorf("a manual installer ran a command: %+v", f.runs)
	}
}

func TestInstallerSudoNeedsSudo(t *testing.T) {
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	ti := newTestInstall(t, installerTarball)
	f := &fakeScripts{}
	ti.in.Script = f.run
	c := ti.cask(t, `[{"installer":[{"script":{"executable":"setup.sh","sudo":true}}]},{"app":["App.app"]}]`)
	err := ti.in.Install(context.Background(), c)
	if err == nil || err.Error() != "app: The installer artifact requires sudo, but HOMEBREW_NO_SUDO is set." {
		t.Errorf("err = %v", err)
	}
	if len(f.runs) != 0 || exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("something ran or changed before the sudo check")
	}
}

func TestInstallerOnlyCaskIsPlanned(t *testing.T) {
	c := api.Cask{Token: "a", Artifacts: raws(t, `[{"installer":[{"manual":"Setup.app"}]}]`)}
	if _, err := plan(c, where); err != nil {
		t.Errorf("err = %v, want an installer-only cask planned", err)
	}
}

func TestScriptCommand(t *testing.T) {
	r := ScriptRun{Executable: "/s/i.sh", Args: []string{"--a b"}, Path: "/p/bin:/usr/bin"}
	if got, want := scriptCommand(r, ""), []string{"/usr/bin/env", "/s/i.sh", "--a b"}; !slices.Equal(got, want) {
		t.Errorf("command = %q, want %q", got, want)
	}
	r.Sudo = true
	if got, want := scriptCommand(r, ""), []string{"/usr/bin/sudo", "-E", "PATH=/p/bin:/usr/bin", "--", "/s/i.sh", "--a b"}; !slices.Equal(got, want) {
		t.Errorf("sudo command = %q, want %q", got, want)
	}
	if got := scriptCommand(r, "/askpass"); got[1] != "-A" {
		t.Errorf("with SUDO_ASKPASS: %q", got)
	}
}

func TestShellEscape(t *testing.T) {
	for in, want := range map[string]string{"plain/a-b.c": "plain/a-b.c", "a b": `a\ b`, "": "''", "it's": `it\'s`} {
		if got := shellEscape(in); got != want {
			t.Errorf("shellEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
