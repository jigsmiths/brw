package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

// Spec is one command to run under a profile; the executable is left open on InheritanceFD for the child to find.
// On macOS the child runs on a pseudo-terminal, so its stdout and stderr arrive merged on Stdout.
type Spec struct {
	Profile string
	Exe     string
	Args    []string
	Env     []string
	Dir     string
	Stdin   io.Reader
	// PassStdin sends Stdin to the child, as brew does unless a parent prompt needs the terminal.
	PassStdin bool
	Stdout    io.Writer
	Stderr    io.Writer
}

func (s Spec) Argv() []string {
	return append([]string{Exec, "-p", s.Profile, s.Exe}, s.Args...)
}

// Run returns the child's exit code, which is not an error by itself.
func Run(ctx context.Context, s Spec) (int, error) {
	exe, err := os.Open(s.Exe)
	if err != nil {
		return 0, err
	}
	defer func() { _ = exe.Close() }()
	argv := s.Argv()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = s.Env, s.Dir
	cmd.ExtraFiles = make([]*os.File, InheritanceFD-2)
	cmd.ExtraFiles[InheritanceFD-3] = exe
	err = execute(cmd, s)
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}
