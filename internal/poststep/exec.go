package poststep

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path"
	"strings"
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
			if _, err := r.inside(p); err != nil {
				return err
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

// run executes a command. Brew runs these steps in a sandbox; here they run with
// the user's own rights, so the command is trusted as much as the bottle is.
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
	cmd := exec.Command(program, args...)
	cmd.Env = os.Environ()
	for k, v := range s.Env {
		cmd.Env = append(cmd.Env, k+"="+r.expand(v))
	}
	if s.Chdir != nil {
		dir, err := r.resolve(*s.Chdir)
		if err != nil {
			return err
		}
		cmd.Dir = dir
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
		cmd.Stdin = bytes.NewReader(b)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if !s.SuppressStderr && r.c.Log != nil {
		_, _ = r.c.Log.Write(stderr.Bytes())
	}
	if err != nil {
		if s.AllowFailure {
			return nil
		}
		return fmt.Errorf("%s: %w: %s", program, err, strings.TrimSpace(stderr.String()))
	}
	if s.StdoutPath == nil {
		return nil
	}
	_, rel, err := r.target(s.StdoutPath)
	if err != nil {
		return err
	}
	if err := r.c.Root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := r.removeFile(rel); err != nil {
		return err
	}
	return r.c.Root.WriteFile(rel, stdout.Bytes(), 0o644)
}
