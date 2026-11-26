package cask

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jigsmiths/brw/internal/sudo"
)

func (l sudoLink) installSteps(t *testing.T, steps string) error {
	t.Helper()
	return l.ti.in.Install(context.Background(), l.ti.cask(t, `[{"app":["App.app"]},{"postflight_steps":[{"steps":[`+steps+`]}]}]`))
}

// recordRuns replaces the runner so that no command runs, not even the fake's.
func (l sudoLink) recordRuns() *[]sudo.Command {
	var got []sudo.Command
	l.ti.in.Run = func(_ context.Context, c sudo.Command) (sudo.Result, error) {
		got = append(got, c)
		return sudo.Result{}, nil
	}
	return &got
}

func sudoRun(t *testing.T, runs []sudo.Command, exe string) sudo.Command {
	t.Helper()
	for _, c := range runs {
		if c.Exe == exe {
			return c
		}
	}
	t.Fatalf("no command ran %s: %v", exe, runs)
	return sudo.Command{}
}

func TestPostflightRunWithSudoKeepsEnvInputDirAndOutputSettings(t *testing.T) {
	l := newSudoLink(t)
	runs := l.recordRuns()
	work := t.TempDir()
	in := filepath.Join(t.TempDir(), "in.txt")
	if err := os.WriteFile(in, []byte("typed"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := l.installSteps(t, fmt.Sprintf(`{"type":"run","sudo":true,"command":{"path":"/usr/sbin/tool"},"args":["{{token}}"],
		"env":{"MODE":"fast"},"stdin_path":{"path":%q},"chdir":{"path":%q},"print_stdout":true,"suppress_stderr":true}`, in, work))
	if err != nil {
		t.Fatal(err)
	}
	c := sudoRun(t, *runs, "/usr/sbin/tool")
	if c.Privilege != sudo.Sudo || !c.MustSucceed || !slices.Equal(c.Args, []string{"app"}) {
		t.Errorf("command = %v", c)
	}
	if !slices.Equal(c.Env, []string{"MODE=fast"}) || c.Input != "typed" || c.Dir != work {
		t.Errorf("env = %q, input = %q, dir = %q", c.Env, c.Input, c.Dir)
	}
	if c.Stdout != io.Writer(l.ti.out) || c.Stderr != io.Discard {
		t.Errorf("stdout = %v, stderr = %v", c.Stdout, c.Stderr)
	}
}

func TestPostflightRunWithSudoRefusesWhenSudoIsDisabled(t *testing.T) {
	l := newSudoLink(t)
	l.recordRuns()
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	err := l.installSteps(t, `{"type":"run","sudo":true,"command":{"path":"/usr/sbin/tool"}}`)
	want := "app: The postflight_steps artifact requires sudo, but HOMEBREW_NO_SUDO is set."
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func TestPostflightRemoveWithSudoRemovesThroughGainedPermissions(t *testing.T) {
	l := newSudoLink(t)
	victim := filepath.Join(l.dir, "victim")
	if err := os.WriteFile(victim, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.installSteps(t, fmt.Sprintf(`{"type":"remove","sudo":true,"paths":[{"path":%q}]}`, victim)); err != nil {
		t.Fatal(err)
	}
	if exists(victim) {
		t.Error("the path was not removed")
	}
}

func TestSudoArtifactIgnoresIfNeededRemove(t *testing.T) {
	l := newSudoLink(t)
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	victim := filepath.Join(l.dir, "victim")
	if err := os.WriteFile(victim, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.installSteps(t, fmt.Sprintf(`{"type":"remove","sudo":"if_needed","paths":[{"path":%q}]}`, victim)); err != nil {
		t.Fatalf("an optional sudo must not demand sudo up front: %v", err)
	}
}
