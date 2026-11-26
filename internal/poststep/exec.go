package poststep

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/sudo"
)

func (r *runner) setPermissions(s step) error {
	var paths []string
	for _, spec := range s.Paths {
		matches, err := r.glob(spec)
		if err != nil {
			return err
		}
		for _, p := range matches {
			if _, err := os.Stat(p); err != nil {
				continue
			}
			paths = append(paths, p)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	args := []string{}
	if !s.NonRecursive {
		args = append(args, "-R")
	}
	args = append(args, "--", s.Permissions)
	args = append(args, paths...)
	out, err := exec.Command("chmod", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("chmod %s: %w: %s", s.Permissions, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// run has no sandbox: the command gets the user's rights, so it is as trusted as the bottle.
func (r *runner) run(s step) error {
	if s.Command == nil {
		return errors.New("the step has no command")
	}
	var program string
	if s.Command.Base == "" || s.Command.Base == "relative" {
		program = r.expand(s.Command.Path)
	} else {
		var err error
		if program, err = r.resolve(*s.Command); err != nil {
			return err
		}
	}
	args := make([]string, len(s.Args))
	for i, a := range s.Args {
		args[i] = r.expand(a)
	}
	env, err := r.environment(s)
	if err != nil {
		return err
	}
	var dir, input string
	if s.Chdir != nil {
		if dir, err = r.resolve(*s.Chdir); err != nil {
			return err
		}
	}
	if s.StdinPath != nil {
		in, err := r.resolve(*s.StdinPath)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(in)
		if err != nil {
			return err
		}
		input = string(b)
	}
	if s.Sudo == sudoAlways {
		return r.runWithSudo(s, sudo.Command{Exe: program, Args: args, Env: env, Input: input, Dir: dir})
	}
	cmd := exec.Command(program, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Dir = dir
	if s.StdinPath != nil {
		cmd.Stdin = strings.NewReader(input)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = cmd.Run()
	if s.PrintStdout && r.c.Log != nil {
		_, _ = r.c.Log.Write(stdout.Bytes())
	}
	if w := r.stderr(); !s.SuppressStderr && w != nil {
		_, _ = w.Write(stderr.Bytes())
	}
	if err != nil {
		if s.AllowFailure {
			return nil
		}
		return fmt.Errorf("%s: %w: %s", program, err, strings.TrimSpace(stderr.String()))
	}
	return r.captureStdout(s, stdout.String())
}

var environmentName = regexp.MustCompile(`^[A-Za-z_]\w*$`)

func (r *runner) environment(s step) ([]string, error) {
	var env []string
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		if !environmentName.MatchString(k) {
			return nil, fmt.Errorf("Invalid variable name: %s", k) //nolint:staticcheck // brew's message, shown to the user
		}
		env = append(env, k+"="+r.expand(s.Env[k]))
	}
	return env, nil
}

func (r *runner) runWithSudo(s step, c sudo.Command) error {
	c.Privilege, c.MustSucceed = sudo.Sudo, !s.AllowFailure
	if s.PrintStdout {
		c.Stdout = r.c.Log
	}
	c.Stderr = r.stderr()
	if s.SuppressStderr {
		c.Stderr = io.Discard
	}
	res, err := r.sudoCommand(c)
	if err != nil || res.Failed {
		return err
	}
	return r.captureStdout(s, res.Stdout)
}

func (r *runner) stderr() io.Writer {
	if r.c.Err != nil {
		return r.c.Err
	}
	return r.c.Log
}

func (r *runner) captureStdout(s step, stdout string) error {
	if s.StdoutPath == nil {
		return nil
	}
	abs, err := r.target(s.StdoutPath)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(abs)); err != nil {
		return err
	}
	if err := r.removeAny(abs); err != nil {
		return err
	}
	return r.writeFile(abs, []byte(stdout), 0o644)
}
