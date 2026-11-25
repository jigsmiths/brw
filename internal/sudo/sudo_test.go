package sudo

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func unsetenv(t *testing.T, name string) {
	t.Helper()
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
}

func TestArgv(t *testing.T) {
	unsetenv(t, "SUDO_ASKPASS")
	t.Setenv("HOMEBREW_SUDO_THROUGH_SUDO_USER", "")
	c := Command{Exe: "/s/i.sh", Args: []string{"--a b"}, Env: []string{"PATH=/p/bin:/usr/bin", "X=a b"}}
	check := func(name string, want []string) {
		t.Helper()
		got, err := c.Argv()
		if err != nil || !slices.Equal(got, want) {
			t.Errorf("%s: argv = %q, %v, want %q", name, got, err, want)
		}
	}
	check("user", []string{"/usr/bin/env", "/s/i.sh", "--a b"})
	c.Privilege = Sudo
	check("sudo", []string{"/usr/bin/sudo", "-E", "PATH=/p/bin:/usr/bin", `X=a\ b`, "--", "/s/i.sh", "--a b"})
	c.Privilege = Root
	check("root", []string{"/usr/bin/sudo", "-u", "root", "-E", "PATH=/p/bin:/usr/bin", `X=a\ b`, "--", "/s/i.sh", "--a b"})
	t.Setenv("SUDO_ASKPASS", "/askpass")
	c.Privilege = Sudo
	if got, _ := c.Argv(); got[1] != "-A" {
		t.Errorf("with SUDO_ASKPASS: %q", got)
	}
	unsetenv(t, "SUDO_ASKPASS")

	t.Setenv("HOMEBREW_SUDO_THROUGH_SUDO_USER", "1")
	t.Setenv("HOMEBREW_SUDO_USER", "bob")
	c = Command{Exe: "/s/i.sh", Env: []string{"A=1"}, Privilege: Root}
	check("through sudo user", []string{"/usr/bin/sudo", "--prompt", "Password for %p:", "-u", "bob", "-E", "A=1", "--", "/usr/bin/sudo",
		"-u", "root", "-E", "A=1", "--", "/s/i.sh"})
	t.Setenv("HOMEBREW_SUDO_USER", " ")
	if _, err := c.Argv(); err == nil || err.Error() != "`$HOMEBREW_SUDO_THROUGH_SUDO_USER` set but `$SUDO_USER` unset!" {
		t.Errorf("err = %v", err)
	}
	c.Privilege = User
	if _, err := c.Argv(); err != nil {
		t.Errorf("a user command needs no sudo user: %v", err)
	}
}

func TestExecRefusesSudoWhenDisabled(t *testing.T) {
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	_, err := Exec(context.Background(), Command{Exe: "/usr/sbin/installer", Args: []string{"-pkg", "a b"}, Privilege: Root})
	want := "Failure while executing; `/usr/sbin/installer -pkg a\\ b` exited with 1. Here's the output:\nsudo is disabled by HOMEBREW_NO_SUDO.\n"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func fakeSudo(t *testing.T) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sudo")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := binary
	binary = path
	t.Cleanup(func() { binary = old })
}

func TestAvailable(t *testing.T) {
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
			var calls []string
			run := func(_ context.Context, c Command) (Result, error) {
				calls = append(calls, c.Exe+" "+strings.Join(c.Args, " "))
				msg := tc.resetErr
				if slices.Contains(c.Args, "-l") {
					msg = tc.lsErr
				}
				if msg != "" {
					return Result{}, errors.New(msg)
				}
				return Result{}, nil
			}
			if got := Available(context.Background(), run); got != tc.want {
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

func TestEnvTrue(t *testing.T) {
	for value, want := range map[string]bool{"": false, " ": false, "1": true, "yes": true, "0": false, "False": false, "OFF": false} {
		t.Setenv("SUDO_TEST_FLAG", value)
		if got := EnvTrue("SUDO_TEST_FLAG"); got != want {
			t.Errorf("EnvTrue(%q) = %v, want %v", value, got, want)
		}
	}
}

func TestColorEnabled(t *testing.T) {
	for name, tc := range map[string]struct {
		env  map[string]string
		want bool
	}{
		"plain buffer":      {want: false},
		"forced":            {env: map[string]string{"HOMEBREW_COLOR": "1"}, want: true},
		"no colour wins":    {env: map[string]string{"HOMEBREW_COLOR": "1", "HOMEBREW_NO_COLOR": "1"}, want: false},
		"NO_COLOR fallback": {env: map[string]string{"HOMEBREW_COLOR": "1", "NO_COLOR": "1"}, want: false},
	} {
		t.Run(name, func(t *testing.T) {
			for _, k := range []string{"HOMEBREW_COLOR", "HOMEBREW_NO_COLOR", "NO_COLOR"} {
				t.Setenv(k, tc.env[k])
			}
			if got := ColorEnabled(&bytes.Buffer{}); got != tc.want {
				t.Errorf("colour = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLineSinkRedactsSecretsAsBrewDoes(t *testing.T) {
	var out bytes.Buffer
	var log outputLog
	sink := newLineSink([]string{"s3cr3t", "tok"}, &log, &out, false)
	_, _ = sink.Write([]byte("key=s3cr3t and\npartial s3"))
	_, _ = sink.Write([]byte("cr3t tok"))
	sink.Flush()
	want := "key=****** and\npartial ****** ******"
	if out.String() != want || log.render(true) != want {
		t.Errorf("out = %q, log = %q, want %q", out.String(), log.render(true), want)
	}
}

func TestEmptySecretRedactsBetweenCharacters(t *testing.T) {
	if got, want := Redact("ab\n", []string{""}), "******a******b******\n******"; got != want {
		t.Errorf("redact = %q, want %q", got, want)
	}
}

func TestSensitiveValues(t *testing.T) {
	t.Setenv("MY_API_TOKEN", "s3cr3t")
	t.Setenv("EMPTY_TOKEN", "")
	t.Setenv("ORDINARY", "ordinary")
	got := SensitiveValues()
	if !slices.Contains(got, "s3cr3t") || !slices.Contains(got, "") || slices.Contains(got, "ordinary") {
		t.Errorf("secrets = %q", got)
	}
}

func TestOutputLogColoursStderrLines(t *testing.T) {
	var log outputLog
	newLineSink(nil, &log, nil, false).emit("out\n")
	newLineSink(nil, &log, nil, true).emit("err\n")
	if got, want := log.render(true), "out\n\x1b[31merr\n\x1b[0m"; got != want {
		t.Errorf("coloured = %q, want %q", got, want)
	}
	if got, want := log.render(false), "out\nerr\n"; got != want {
		t.Errorf("plain = %q, want %q", got, want)
	}
	if log.stream(false) != "out\n" || log.stream(true) != "err\n" {
		t.Errorf("streams = %q, %q", log.stream(false), log.stream(true))
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

func TestTerminalColumns(t *testing.T) {
	size := func(w int, ok bool) func() (int, bool) { return func() (int, bool) { return w, ok } }
	tput := func(out string) func() string { return func() string { return out } }
	for name, tc := range map[string]struct {
		size func() (int, bool)
		tput func() string
		want int
	}{
		"terminal":      {size(132, true), tput("40\n"), 132},
		"tput":          {size(0, false), tput("100\n"), 100},
		"tput is blank": {size(0, false), tput(""), 80},
		"tput is junk":  {size(0, false), tput("many"), 0},
	} {
		if got := columns(tc.size, tc.tput); got != tc.want {
			t.Errorf("%s: columns = %d, want %d", name, got, tc.want)
		}
	}
}

func TestSignalNames(t *testing.T) {
	for sig, want := range map[syscall.Signal]string{
		syscall.SIGINT: "INT", syscall.SIGUSR1: "USR1", syscall.SIGTRAP: "TRAP", syscall.SIGSTOP: "STOP",
		syscall.SIGWINCH: "WINCH", syscall.SIGCHLD: "CHLD", syscall.SIGSYS: "SYS", syscall.SIGXCPU: "XCPU",
		syscall.SIGVTALRM: "VTALRM", syscall.SIGIO: "IO", syscall.SIGABRT: "ABRT",
	} {
		if got := signalName(sig); got != want {
			t.Errorf("signalName(%d) = %q, want %q", sig, got, want)
		}
	}
}

func TestParseSignal(t *testing.T) {
	for name, want := range map[string]syscall.Signal{
		"TERM": syscall.SIGTERM, "SIGTERM": syscall.SIGTERM, "KILL": syscall.SIGKILL, "USR2": syscall.SIGUSR2,
		"SIGHUP": syscall.SIGHUP, "EXIT": 0, "SIGEXIT": 0, "WINCH": syscall.SIGWINCH,
	} {
		if got, err := ParseSignal(name); err != nil || got != want {
			t.Errorf("ParseSignal(%q) = %d, %v, want %d", name, got, err, want)
		}
	}
	for name, want := range map[string]string{
		"BOGUS": "unsupported signal 'SIGBOGUS'", "SIGBOGUS": "unsupported signal 'SIGBOGUS'",
		"SIGSIGTERM": "unsupported signal 'SIGSIGTERM'", "term": "unsupported signal 'SIGterm'", "": "unsupported signal 'SIG'",
	} {
		if _, err := ParseSignal(name); err == nil || err.Error() != want {
			t.Errorf("ParseSignal(%q) err = %v, want %q", name, err, want)
		}
	}
}

func TestShellEscape(t *testing.T) {
	for in, want := range map[string]string{"plain/a-b.c": "plain/a-b.c", "a b": `a\ b`, "": "''", "it's": `it\'s`} {
		if got := ShellEscape(in); got != want {
			t.Errorf("ShellEscape(%q) = %q, want %q", in, got, want)
		}
	}
}
