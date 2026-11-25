package cask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
)

const (
	execBit     = 0o111
	envCommand  = "/usr/bin/env"
	sudoCommand = "/usr/bin/sudo"
)

type installerStep struct {
	manual                   bool
	path                     string
	args                     []string
	input                    string
	mustSucceed, sudo        bool
	printStdout, printStderr bool
	ignored                  []string
}

// ScriptRun is one installer script as brew's SystemCommand runs it.
type ScriptRun struct {
	Executable               string
	Args                     []string
	Input                    string
	Path                     string
	MustSucceed, Sudo        bool
	PrintStdout, PrintStderr bool
	Stdout, Stderr           io.Writer
}

type ScriptRunner func(ctx context.Context, r ScriptRun) error

var scriptKeys = []string{"args", "input", "executable", "must_succeed", "sudo", "print_stdout", "print_stderr"}

func readInstallers(token string, args json.RawMessage) ([]installerStep, error) {
	invalid := func(format string, a ...any) error {
		return brewError("Cask '%s' definition is invalid: %s", token, fmt.Sprintf(format, a...))
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(args, &list); err != nil {
		return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
	}
	var steps []installerStep
	for _, el := range list {
		if len(el) != 1 {
			return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
		}
		if raw, ok := el["manual"]; ok {
			var path string
			if json.Unmarshal(raw, &path) != nil {
				return nil, invalid("installer requires a path for :manual")
			}
			steps = append(steps, installerStep{manual: true, path: path})
			continue
		}
		raw, ok := el["script"]
		if !ok {
			return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
		}
		s := installerStep{mustSucceed: true, printStdout: true, printStderr: true}
		var fields map[string]json.RawMessage
		var executable string
		if json.Unmarshal(raw, &executable) == nil {
			s.path = executable
			steps = append(steps, s)
			continue
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
		}
		for key, v := range fields {
			if !slices.Contains(scriptKeys, key) {
				s.ignored = append(s.ignored, key)
			}
			if err := s.set(key, v); err != nil {
				return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
			}
		}
		slices.Sort(s.ignored)
		if s.path == "" {
			return nil, invalid("installer missing executable")
		}
		steps = append(steps, s)
	}
	return steps, nil
}

func (s *installerStep) set(key string, v json.RawMessage) error {
	switch key {
	case "executable":
		return json.Unmarshal(v, &s.path)
	case "must_succeed":
		return json.Unmarshal(v, &s.mustSucceed)
	case "sudo":
		return json.Unmarshal(v, &s.sudo)
	case "print_stdout":
		return json.Unmarshal(v, &s.printStdout)
	case "print_stderr":
		return json.Unmarshal(v, &s.printStderr)
	case "args":
		var list []any
		if err := json.Unmarshal(v, &list); err != nil {
			return err
		}
		for _, a := range list {
			s.args = append(s.args, fmt.Sprint(a))
		}
	case "input":
		var one string
		if json.Unmarshal(v, &one) == nil {
			s.input = one
			return nil
		}
		var many []string
		if err := json.Unmarshal(v, &many); err != nil {
			return err
		}
		s.input = strings.Join(many, "")
	}
	return nil
}

func (in *Installer) runInstallers(ctx context.Context, token, staged string, steps []installerStep) error {
	for _, s := range steps {
		if s.manual {
			in.logf("Cask %s only provides a manual installer. To run it and complete the installation:\n  open %s",
				token, shellEscape(joinName(staged, s.path)))
			continue
		}
		if len(s.ignored) > 0 {
			symbols := make([]string, len(s.ignored))
			for i, k := range s.ignored {
				symbols[i] = ":" + k
			}
			in.warnf("Unknown arguments to installer -- [%s] (ignored). Running `brew update; brew cleanup` will likely fix it.",
				strings.Join(symbols, ", "))
		}
		in.logf("==> Running installer script '%s'", s.path)
		exe, err := in.stagedExecutable(staged, s.path)
		if err != nil {
			return fmt.Errorf("%s: %w", token, err)
		}
		r := ScriptRun{
			Executable: exe, Args: s.args, Input: s.input, MustSucceed: s.mustSucceed, Sudo: s.sudo,
			PrintStdout: s.printStdout, PrintStderr: s.printStderr,
			Path: strings.Join([]string{filepath.Join(in.Where.Prefix, "bin"), filepath.Join(in.Where.Prefix, "sbin"), os.Getenv("PATH")}, ":"),
		}
		if s.printStdout {
			r.Stdout = in.Log
		}
		if s.printStderr {
			r.Stderr = in.Warn
		}
		run := in.Script
		if run == nil {
			run = execScript
		}
		if err := run(ctx, r); err != nil {
			return fmt.Errorf("%s: %w", token, err)
		}
	}
	return nil
}

func (in *Installer) stagedExecutable(staged, path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		path = filepath.Join(in.Where.Home, path[1:])
	}
	abs := path
	if !filepath.IsAbs(path) {
		abs = filepath.Join(staged, path)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return path, nil
	}
	if syscall.Access(abs, 1) != nil {
		if err := in.chmodAll(abs, fi.Mode().Perm()|execBit); err != nil {
			return "", err
		}
	}
	return abs, nil
}

func (in *Installer) chmodAll(path string, perm os.FileMode) error {
	if in.inside(in.Where.Prefix, path) {
		rel, err := filepath.Rel(in.Where.Prefix, path)
		if err != nil {
			return err
		}
		return in.Root.Chmod(rel, perm)
	}
	return os.Chmod(path, perm) //fsx:allow brew makes an installer script outside the prefix executable
}

func scriptCommand(r ScriptRun, askpass string) []string {
	if !r.Sudo {
		return append([]string{envCommand, r.Executable}, r.Args...)
	}
	cmd := []string{sudoCommand}
	if askpass != "" {
		cmd = append(cmd, "-A")
	}
	cmd = append(cmd, "-E", "PATH="+shellEscape(r.Path), "--", r.Executable)
	return append(cmd, r.Args...)
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func execScript(ctx context.Context, r ScriptRun) error {
	argv := scriptCommand(r, os.Getenv("SUDO_ASKPASS"))
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+r.Path)
	if r.Input != "" {
		cmd.Stdin = strings.NewReader(r.Input)
	}
	var captured bytes.Buffer
	log := &lockedWriter{w: &captured}
	cmd.Stdout, cmd.Stderr = log, log
	if r.PrintStdout && r.Stdout != nil {
		cmd.Stdout = io.MultiWriter(log, &lockedWriter{w: r.Stdout})
	}
	if r.PrintStderr && r.Stderr != nil {
		cmd.Stderr = io.MultiWriter(log, &lockedWriter{w: r.Stderr})
	}
	err := cmd.Run()
	if exitErr, ok := errors.AsType[*exec.ExitError](err); ok {
		if !r.MustSucceed {
			return nil
		}
		parts := make([]string, len(argv))
		for i, a := range argv {
			parts[i] = shellEscape(a)
		}
		msg := fmt.Sprintf("Failure while executing; `%s` exited with %d.", strings.ReplaceAll(strings.Join(parts, " "), `\=`, "="), exitErr.ExitCode())
		if captured.Len() > 0 {
			msg += " Here's the output:\n" + strings.TrimSuffix(captured.String(), "\n") + "\n"
		}
		return brewMessage(msg)
	}
	return err
}

// shellEscape is Ruby's String#shellescape.
func shellEscape(s string) string {
	if s == "" {
		return "''"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString("'\n'")
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("_-.,:+/@", r):
			b.WriteRune(r)
		default:
			b.WriteByte('\\')
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (p installPlan) needsSudo() bool {
	return slices.ContainsFunc(p.installers, func(s installerStep) bool { return s.sudo })
}
