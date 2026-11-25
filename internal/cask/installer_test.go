package cask

import (
	"bytes"
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
	t.Setenv("MY_API_TOKEN", "s3cr3t")
	t.Setenv("ORDINARY", "ordinary")
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
		Stdout: ti.out, Stderr: ti.warn,
		Path: ti.prefix + "/bin:" + ti.prefix + "/sbin:/usr/bin:/bin",
	}
	if len(f.runs) == 1 {
		if !slices.Contains(f.runs[0].Secrets, "s3cr3t") || slices.Contains(f.runs[0].Secrets, "ordinary") {
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

func TestInstallerWarnsAboutArgumentsAsBrewDoes(t *testing.T) {
	ti := newTestInstall(t, installerTarball)
	ti.in.Script = (&fakeScripts{}).run
	c := ti.cask(t, `[{"installer":[{"script":{"executable":"setup.sh","zeta":1,"print_stdout":false,"alpha":2}}]},{"app":["App.app"]}]`)
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Warning: Unknown arguments to installer -- [:zeta, :alpha] (ignored). Running `brew update; brew cleanup` will likely fix it.\n",
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
		ti.in.Script = f.run
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

func TestInstallerSudoThroughSudoUser(t *testing.T) {
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	t.Setenv("HOMEBREW_SUDO_THROUGH_SUDO_USER", "1")
	t.Setenv("HOMEBREW_SUDO_USER", "")
	ti := newTestInstall(t, installerTarball)
	f := &fakeScripts{}
	ti.in.Script = f.run
	c := ti.cask(t, `[{"installer":[{"script":{"executable":"setup.sh","sudo":true}}]},{"app":["App.app"]}]`)
	err := ti.in.Install(context.Background(), c)
	if err == nil || !strings.Contains(err.Error(), "`$HOMEBREW_SUDO_THROUGH_SUDO_USER` set but `$SUDO_USER` unset!") {
		t.Errorf("err = %v", err)
	}
	t.Setenv("HOMEBREW_SUDO_USER", "bob")
	ti = newTestInstall(t, installerTarball)
	ti.in.Script = f.run
	if err := ti.in.Install(context.Background(), ti.cask(t, `[{"installer":[{"script":{"executable":"setup.sh","sudo":true}}]},{"app":["App.app"]}]`)); err != nil {
		t.Fatal(err)
	}
	if got := f.runs[len(f.runs)-1].SudoUser; got != "bob" {
		t.Errorf("sudo user = %q, want bob", got)
	}
}

func fakeSudo(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := sudoBinary
	sudoBinary = path
	t.Cleanup(func() { sudoBinary = old })
}

func TestSudoAvailable(t *testing.T) {
	for name, tc := range map[string]struct {
		noSudo, checked string
		resetErr, lsErr string
		want            bool
		wantCalls       int
		wantNoSudo      string
	}{
		"disabled":          {noSudo: "1", want: false, wantNoSudo: "1"},
		"already checked":   {checked: "1", want: true},
		"works":             {want: true, wantCalls: 2},
		"no new privileges": {resetErr: `The "no new privileges" flag is set`, want: false, wantCalls: 1, wantNoSudo: "1"},
		"setuid missing":    {resetErr: "sudo: /x must be owned by uid 0 and have the setuid bit set", want: false, wantCalls: 1, wantNoSudo: "1"},
		"not a sudoer":      {lsErr: "bob is not in the sudoers file.", want: false, wantCalls: 2, wantNoSudo: "1"},
		"needs a password":  {lsErr: "sudo: a password is required", want: true, wantCalls: 2},
		"reset is odd":      {resetErr: "sudo: something else", want: true, wantCalls: 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOMEBREW_NO_SUDO", tc.noSudo)
			t.Setenv("HOMEBREW_SUDO_CHECKED", tc.checked)
			fakeSudo(t)
			ti := newTestInstall(t, installerTarball)
			var calls []string
			ti.in.Exec = func(_ context.Context, name string, args ...string) ([]byte, error) {
				calls = append(calls, name+" "+strings.Join(args, " "))
				msg := tc.resetErr
				if slices.Contains(args, "-l") {
					msg = tc.lsErr
				}
				if msg != "" {
					return nil, errors.New(msg)
				}
				return nil, nil
			}
			if got := ti.in.sudoAvailable(context.Background()); got != tc.want {
				t.Errorf("available = %v, want %v", got, tc.want)
			}
			if len(calls) != tc.wantCalls {
				t.Errorf("calls = %q, want %d", calls, tc.wantCalls)
			}
			if got := os.Getenv("HOMEBREW_NO_SUDO"); got != tc.wantNoSudo {
				t.Errorf("HOMEBREW_NO_SUDO = %q, want %q", got, tc.wantNoSudo)
			}
		})
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
	if got, want := scriptCommand(r), []string{"/usr/bin/env", "/s/i.sh", "--a b"}; !slices.Equal(got, want) {
		t.Errorf("command = %q, want %q", got, want)
	}
	r.Sudo = true
	if got, want := scriptCommand(r), []string{"/usr/bin/sudo", "-E", "PATH=/p/bin:/usr/bin", "--", "/s/i.sh", "--a b"}; !slices.Equal(got, want) {
		t.Errorf("sudo command = %q, want %q", got, want)
	}
	r.Askpass = true
	if got := scriptCommand(r); got[1] != "-A" {
		t.Errorf("with SUDO_ASKPASS: %q", got)
	}
	r.SudoUser = "bob"
	want := []string{"/usr/bin/sudo", "--prompt", "Password for %p:", "-u", "bob", "-A", "-E", "PATH=/p/bin:/usr/bin", "--", "/usr/bin/sudo",
		"-A", "-E", "PATH=/p/bin:/usr/bin", "--", "/s/i.sh", "--a b"}
	if got := scriptCommand(r); !slices.Equal(got, want) {
		t.Errorf("sudo user command = %q, want %q", got, want)
	}
}

func TestLineSinkRedactsSecretsAsBrewDoes(t *testing.T) {
	var out bytes.Buffer
	var log outputLog
	sink := newLineSink([]string{"s3cr3t", "tok"}, &log, &out)
	_, _ = sink.Write([]byte("key=s3cr3t and\npartial s3"))
	_, _ = sink.Write([]byte("cr3t tok"))
	sink.Flush()
	want := "key=****** and\npartial ****** ******"
	if out.String() != want || log.String() != want {
		t.Errorf("out = %q, log = %q, want %q", out.String(), log.String(), want)
	}
}

func TestFailureMessage(t *testing.T) {
	got := failureMessage([]string{"/usr/bin/env", "/s/i.sh", "--key=s3cr3t"}, "exited with 3", "oops s3cr3t\n", []string{"s3cr3t"})
	want := "Failure while executing; `/usr/bin/env /s/i.sh --key=******` exited with 3. Here's the output:\noops s3cr3t\n"
	if got != want {
		t.Errorf("message = %q, want %q", got, want)
	}
	if got := failureMessage([]string{"x"}, "was terminated by uncaught signal INT", "", nil); got != "Failure while executing; `x` was terminated by uncaught signal INT." {
		t.Errorf("message = %q", got)
	}
}

func TestShellEscape(t *testing.T) {
	for in, want := range map[string]string{"plain/a-b.c": "plain/a-b.c", "a b": `a\ b`, "": "''", "it's": `it\'s`} {
		if got := shellEscape(in); got != want {
			t.Errorf("shellEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
