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
	"github.com/jigsmiths/brw/internal/sudo"
)

func TestPlanReadsInstallerStanzas(t *testing.T) {
	c := api.Cask{Token: "a", Artifacts: raws(t, `[
		{"installer":[{"script":"setup.sh"}]},
		{"installer":[{"script":{"executable":"i.sh","args":["--quiet",1],"input":["y\n","n\n"],"must_succeed":false,"sudo":true,"zeta":1,"print_stdout":false,"alpha":2,"print_stderr":"debug"}}]},
		{"installer":[{"manual":"Setup.app"}]}]`)}
	p, err := plan(c, where)
	if err != nil {
		t.Fatal(err)
	}
	want := []installerStep{
		{path: "setup.sh", mustSucceed: true, stderr: outputOn},
		{path: "i.sh", args: []string{"--quiet", "1"}, input: "y\nn\n", sudo: true, stderr: outputDebug,
			ignored: []string{"zeta", "alpha"}, overridden: true},
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
	runs []sudo.Command
	err  error
	seen func(sudo.Command)
}

func (f *fakeScripts) run(_ context.Context, c sudo.Command) (sudo.Result, error) {
	f.runs = append(f.runs, c)
	if f.seen != nil {
		f.seen(c)
	}
	return sudo.Result{}, f.err
}

var installerTarball = func() []byte {
	var t testing.T
	return tarGzBytes(&t,
		entry{name: "App.app/Contents/Info.plist", body: "<plist/>"},
		entry{name: "setup.sh", body: "#!/bin/sh\n", mode: 0o644})
}()

func TestInstallRunsInstallerScriptBeforeApps(t *testing.T) {
	t.Setenv("PATH", "/usr/bin:/bin")
	t.Setenv("MY_API_TOKEN", "s3cr3t")
	t.Setenv("ORDINARY", "ordinary")
	t.Setenv("EMPTY_TOKEN", "")
	t.Setenv("HOMEBREW_COLOR", "")
	ti := newTestInstall(t, installerTarball)
	f := &fakeScripts{}
	ti.in.Run = f.run
	f.seen = func(sudo.Command) {
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
	want := sudo.Command{
		Exe: staged, Args: []string{"--quiet"}, Input: "yes\n", MustSucceed: true,
		Stdout: ti.out, Stderr: ti.warn,
		Env: []string{"PATH=" + ti.prefix + "/bin:" + ti.prefix + "/sbin:/usr/bin:/bin"},
	}
	if len(f.runs) == 1 {
		if !slices.Contains(f.runs[0].Secrets, "s3cr3t") || !slices.Contains(f.runs[0].Secrets, "") || slices.Contains(f.runs[0].Secrets, "ordinary") {
			t.Errorf("secrets = %q", f.runs[0].Secrets)
		}
		f.runs[0].Secrets = nil
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
	ti.in.Run = (&fakeScripts{err: errors.New("boom")}).run
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
	ti.in.Run = f.run
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
	ti.in.Run = f.run
	c := ti.cask(t, `[{"installer":[{"script":{"executable":"setup.sh","sudo":true}}]},{"app":["App.app"]}]`)
	err := ti.in.Install(context.Background(), c)
	if err == nil || err.Error() != "app: The installer artifact requires sudo, but HOMEBREW_NO_SUDO is set." {
		t.Errorf("err = %v", err)
	}
	if len(f.runs) != 0 || exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("something ran or changed before the sudo check")
	}
}

func TestInstallerWarnsAboutArgumentsAsBrewDoes(t *testing.T) {
	ti := newTestInstall(t, installerTarball)
	ti.in.Run = (&fakeScripts{}).run
	c := ti.cask(t, `[{"installer":[{"script":{"executable":"setup.sh","zeta":1,"print_stdout":false,"alpha":2}}]},{"app":["App.app"]}]`)
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Warning: Unknown arguments to installer -- [:zeta, :alpha] (ignored). Running `brw update; brw cleanup` will likely fix it.\n",
		"Error: Some arguments to installer will be ignored -- :[:zeta, :alpha] (overridden).\n",
	} {
		if !strings.Contains(ti.warn.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", ti.warn.String(), want)
		}
	}
}

func TestInstallerOutputModes(t *testing.T) {
	for debug, wantStderr := range map[string]bool{"": false, "1": true, "0": false} {
		t.Setenv("HOMEBREW_DEBUG", debug)
		ti := newTestInstall(t, installerTarball)
		f := &fakeScripts{}
		ti.in.Run = f.run
		c := ti.cask(t, `[{"installer":[{"script":{"executable":"setup.sh","print_stderr":"debug"}}]},{"app":["App.app"]}]`)
		if err := ti.in.Install(context.Background(), c); err != nil {
			t.Fatal(err)
		}
		if got := f.runs[0].Stderr != nil; got != wantStderr {
			t.Errorf("HOMEBREW_DEBUG=%q: stderr printed = %v, want %v", debug, got, wantStderr)
		}
		if f.runs[0].Stdout == nil {
			t.Errorf("HOMEBREW_DEBUG=%q: installer stdout is always printed", debug)
		}
	}
}

func TestInstallerRunsSudoStanzasWithSudo(t *testing.T) {
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	t.Setenv("HOMEBREW_NO_SUDO", "")
	ti := newTestInstall(t, installerTarball)
	f := &fakeScripts{}
	ti.in.Run = f.run
	c := ti.cask(t, `[{"installer":[{"script":"setup.sh"},{"script":{"executable":"setup.sh","sudo":true}}]},{"app":["App.app"]}]`)
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if len(f.runs) != 2 || f.runs[0].Privilege != sudo.User || f.runs[1].Privilege != sudo.Sudo {
		t.Errorf("privileges = %+v", f.runs)
	}
}

func TestInstallerOnlyCaskIsPlanned(t *testing.T) {
	c := api.Cask{Token: "a", Artifacts: raws(t, `[{"installer":[{"manual":"Setup.app"}]}]`)}
	if _, err := plan(c, where); err != nil {
		t.Errorf("err = %v, want an installer-only cask planned", err)
	}
}
