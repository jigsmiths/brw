package poststep

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/sudo"
)

func commandFailure(t *testing.T) error {
	t.Helper()
	_, err := sudo.Exec(context.Background(), sudo.Command{Exe: "/bin/false", MustSucceed: true})
	if !sudo.IsFailure(err) {
		t.Fatalf("err = %v, want a command failure", err)
	}
	return err
}

func (f *sudoFixture) only() sudo.Command {
	f.t.Helper()
	if len(f.stub.cmds) != 1 {
		f.t.Fatalf("runner got %d commands, want 1: %v", len(f.stub.cmds), f.stub.cmds)
	}
	return f.stub.cmds[0]
}

func TestRunWithSudoPassesBrewsCommandOptions(t *testing.T) {
	f := newSudoFixture(t)
	f.write("var/in.txt", "typed", 0o644)
	f.mkdir("var/work")
	f.mustRun(`[{"type":"run","sudo":true,"command":{"path":"/usr/sbin/tool"},
		"args":["{{token}}","--v={{version}}"],"env":{"A_B":"x-{{version}}"},
		"stdin_path":{"base":"var","path":"in.txt"},"chdir":{"base":"var","path":"work"}}]`)
	c := f.only()
	if c.Exe != "/usr/sbin/tool" || c.Privilege != sudo.Sudo || !c.MustSucceed {
		t.Errorf("runner got %v, want sudo /usr/sbin/tool that must succeed", c)
	}
	if want := []string{"{{token}}", "--v=1.2.3"}; !slices.Equal(c.Args, want) {
		t.Errorf("args = %q, want %q", c.Args, want)
	}
	if want := []string{"A_B=x-1.2.3"}; !slices.Equal(c.Env, want) {
		t.Errorf("env = %q, want %q", c.Env, want)
	}
	if c.Input != "typed" || c.Dir != filepath.Join(f.prefix, "var/work") {
		t.Errorf("input = %q, dir = %q", c.Input, c.Dir)
	}
	if c.Stdout != nil || c.Stderr != io.Writer(&f.log) {
		t.Errorf("stdout %v and stderr %v: brew prints only stderr by default", c.Stdout, c.Stderr)
	}
}

func TestRunWithSudoPrintsStderrToTheErrorStream(t *testing.T) {
	f := newSudoFixture(t)
	var errs bytes.Buffer
	f.ctx.Err = &errs
	f.mustRun(`[{"type":"run","sudo":true,"command":{"path":"/a"}}]`)
	if c := f.only(); c.Stderr != io.Writer(&errs) {
		t.Errorf("stderr = %v, want the error stream", c.Stderr)
	}
}

func TestRunWithoutSudoPrintsStderrToTheErrorStream(t *testing.T) {
	f := newSudoFixture(t)
	var errs bytes.Buffer
	f.ctx.Err = &errs
	f.mustRun(`[{"type":"run","command":{"path":"/bin/sh"},"args":["-c","echo oops >&2"]}]`)
	if errs.String() != "oops\n" || f.log.String() != "" {
		t.Errorf("stderr = %q, log = %q", errs.String(), f.log.String())
	}
}

func TestRunWithSudoExpandsCaskTokensAndResolvesBases(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(`[{"type":"run","sudo":true,"command":{"base":"libexec","path":"tool"},"args":["{{name}}"]}]`)
	c := f.only()
	if want := filepath.Join(f.prefix, f.ctx.Keg, "libexec/tool"); c.Exe != want || !slices.Equal(c.Args, []string{"pkg"}) {
		t.Errorf("runner got %v, want %s", c, want)
	}
}

func TestRunWithSudoPrintsOutputOnlyWhenAsked(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(`[{"type":"run","sudo":true,"command":{"path":"/a"},"print_stdout":true,"suppress_stderr":true}]`)
	c := f.only()
	if c.Stdout != io.Writer(&f.log) {
		t.Errorf("stdout = %v, want the log", c.Stdout)
	}
	if c.Stderr != io.Discard {
		t.Errorf("stderr = %v, want it discarded", c.Stderr)
	}
}

func TestRunWithSudoAllowFailure(t *testing.T) {
	f := newSudoFixture(t)
	t.Setenv("HOMEBREW_NO_SUDO", "")
	step := func(extra string) string {
		return `[{"type":"run","sudo":true,"command":{"path":"/a"}` + extra + `}]`
	}
	f.mustRun(step(``))
	f.mustRun(step(`,"allow_failure":true`))
	if got := f.stub.cmds; !got[0].MustSucceed || got[1].MustSucceed {
		t.Errorf("must_succeed = %t then %t, brew passes must_succeed: allow_failure != true", got[0].MustSucceed, got[1].MustSucceed)
	}
	f.stub.failed = true
	f.mustRun(step(`,"allow_failure":true`))
	f.stub.err = commandFailure(t)
	if err := f.run(step(`,"allow_failure":true`)); !errors.Is(err, f.stub.err) {
		t.Errorf("err = %v, an error the runner returns is not an unsuccessful exit and must propagate", err)
	}
}

func TestRunWithSudoAllowFailureStillFailsWhenSudoIsDisabled(t *testing.T) {
	f := newSudoFixture(t)
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	f.ctx.Sudo = sudo.Exec
	err := f.run(`[{"type":"run","sudo":true,"allow_failure":true,"command":{"path":"/a"}}]`)
	if err == nil || !strings.Contains(err.Error(), "sudo is disabled by HOMEBREW_NO_SUDO") {
		t.Errorf("err = %v, brew raises when sudo is disabled, whatever must_succeed says", err)
	}
}

func TestRunWithSudoWritesStdoutOnlyOnSuccess(t *testing.T) {
	f := newSudoFixture(t)
	t.Setenv("HOMEBREW_NO_SUDO", "")
	step := `[{"type":"run","sudo":true,"allow_failure":true,"command":{"path":"/a"},"stdout_path":{"base":"var","path":"captured/out.txt"}}]`
	f.stub.failed, f.stub.stdout = true, "ignored"
	f.mustRun(step)
	if f.exists("var/captured/out.txt") {
		t.Error("stdout of a failed command was written")
	}
	f.stub.failed = false
	f.stub.stdout = "output\n"
	f.mustRun(step)
	if got := f.read("var/captured/out.txt"); got != "output\n" {
		t.Errorf("captured = %q", got)
	}
}

func TestRunIfNeededIsNotSudo(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(`[{"type":"run","sudo":"if_needed","command":{"path":"true"}}]`)
	if len(f.stub.cmds) != 0 {
		t.Errorf("brew runs a step with sudo %q as the user, got %v", "if_needed", f.stub.cmds)
	}
}

func TestRunRejectsInvalidEnvironmentNames(t *testing.T) {
	f := newSudoFixture(t)
	for _, name := range []string{"1A", "A-B", "A B", ""} {
		err := f.run(fmt.Sprintf(`[{"type":"run","sudo":true,"command":{"path":"/a"},"env":{%q:"x"}}]`, name))
		if err == nil || !strings.Contains(err.Error(), "Invalid variable name") {
			t.Errorf("%q: err = %v", name, err)
		}
	}
	if len(f.stub.cmds) != 0 {
		t.Errorf("runner got %v", f.stub.cmds)
	}
}

func TestRunPrintsStdoutWithoutSudo(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(`[{"type":"run","print_stdout":true,"command":{"path":"echo"},"args":["hello"]}]`)
	if got := f.log.String(); got != "hello\n" {
		t.Errorf("log = %q", got)
	}
	f.log.Reset()
	f.mustRun(`[{"type":"run","command":{"path":"echo"},"args":["hello"]}]`)
	if f.log.Len() != 0 {
		t.Errorf("log = %q, brew prints stdout only when asked", f.log.String())
	}
}

func TestRunGuardsSkipASudoStep(t *testing.T) {
	f := newSudoFixture(t)
	p := filepath.Join(f.dir, "present")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"run","sudo":true,"command":{"path":"/a"},"guards":[{"path":%q,"condition":"unless_exists","id":"1"}]}]`, p))
	if len(f.stub.cmds) != 0 {
		t.Errorf("a step whose guard fails ran: %v", f.stub.cmds)
	}
}
