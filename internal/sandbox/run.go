package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
)

type Spec struct {
	Profile     string
	Exe         string
	Args        []string
	Env         []string
	Dir         string
	Stdin       io.Reader
	Stdout      io.Writer
	Stderr      io.Writer
	Inheritance *os.File
}

func (s Spec) Argv() []string {
	return append([]string{Exec, "-p", s.Profile, s.Exe}, s.Args...)
}

// Run returns the child's exit code, which is not an error by itself.
func Run(ctx context.Context, s Spec) (int, error) {
	argv := s.Argv()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env, cmd.Dir, cmd.Stdin, cmd.Stdout, cmd.Stderr = s.Env, s.Dir, s.Stdin, s.Stdout, s.Stderr
	if s.Inheritance != nil {
		cmd.ExtraFiles = make([]*os.File, InheritanceFD-2)
		cmd.ExtraFiles[InheritanceFD-3] = s.Inheritance
	}
	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		return exitErr.ExitCode(), nil
	}
	return 0, err
}
