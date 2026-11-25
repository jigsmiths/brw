// Package sudo runs commands the way Homebrew's SystemCommand does, with or without sudo.
package sudo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

const (
	envCommand  = "/usr/bin/env"
	sudoCommand = "/usr/bin/sudo"
)

type Privilege int

const (
	User Privilege = iota
	Sudo
	Root
)

type Command struct {
	Exe            string
	Args           []string
	Env            []string
	Input          string
	Privilege      Privilege
	MustSucceed    bool
	Stdout, Stderr io.Writer
	Trace          io.Writer
	Color          bool
	Secrets        []string
}

// Format prints the command for every verb with its secrets redacted and its writers left out.
func (c Command) Format(f fmt.State, _ rune) {
	hide := func(s string) string { return Redact(s, c.Secrets) }
	args := make([]string, len(c.Args))
	for i, a := range c.Args {
		args[i] = hide(a)
	}
	env := make([]string, len(c.Env))
	for i, e := range c.Env {
		env[i] = hide(e)
	}
	_, _ = fmt.Fprintf(f, "sudo.Command{Exe:%s Args:%q Env:%q Input:%q Privilege:%d MustSucceed:%t Secrets:%d}",
		hide(c.Exe), args, env, hide(c.Input), c.Privilege, c.MustSucceed, len(c.Secrets))
}

type Result struct{ Stdout, Stderr string }

type Runner func(ctx context.Context, c Command) (Result, error)

type message string

func (m message) Error() string { return string(m) }

func (c Command) Argv() ([]string, error) {
	if c.Privilege == User {
		return append([]string{envCommand, c.Exe}, c.Args...), nil
	}
	var askpass []string
	if _, ok := os.LookupEnv("SUDO_ASKPASS"); ok {
		askpass = []string{"-A"}
	}
	envArgs := []string{"-E"}
	for _, e := range c.Env {
		name, value, _ := strings.Cut(e, "=")
		envArgs = append(envArgs, ShellEscape(name)+"="+ShellEscape(value))
	}
	envArgs = append(envArgs, "--")
	argv := []string{sudoCommand}
	if EnvTrue("HOMEBREW_SUDO_THROUGH_SUDO_USER") {
		user := os.Getenv("HOMEBREW_SUDO_USER")
		if strings.TrimSpace(user) == "" {
			return nil, message("`$HOMEBREW_SUDO_THROUGH_SUDO_USER` set but `$SUDO_USER` unset!")
		}
		argv = append(argv, "--prompt", "Password for %p:", "-u", user)
		argv = append(argv, askpass...)
		argv = append(argv, envArgs...)
		argv = append(argv, sudoCommand)
	}
	if c.Privilege == Root {
		argv = append(argv, "-u", "root")
	}
	argv = append(argv, askpass...)
	argv = append(argv, envArgs...)
	argv = append(argv, c.Exe)
	return append(argv, c.Args...), nil
}

func Exec(ctx context.Context, c Command) (Result, error) {
	if c.Privilege != User && !Available(ctx, Exec) {
		return Result{}, message(failureMessage(append([]string{c.Exe}, c.Args...), "exited with 1", "sudo is disabled by HOMEBREW_NO_SUDO.\n", c.Secrets))
	}
	argv, err := c.Argv()
	if err != nil {
		return Result{}, err
	}
	if c.Trace != nil {
		_, _ = fmt.Fprintln(c.Trace, Redact(ShellJoin(argv), c.Secrets))
	}
	Guard(argv[0])
	Guard(c.Exe)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), c.Env...)
	cmd.Env = append(cmd.Env, "COLUMNS="+strconv.Itoa(columns(stdinColumns, tputColumns)))
	if c.Input != "" {
		cmd.Stdin = strings.NewReader(c.Input)
	}
	var log outputLog
	stdout, stderr := newLineSink(c.Secrets, &log, c.Stdout, false), newLineSink(c.Secrets, &log, c.Stderr, true)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err = cmd.Run()
	stdout.Flush()
	stderr.Flush()
	result := Result{Stdout: log.stream(false), Stderr: log.stream(true)}
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok || !c.MustSucceed {
		if ok {
			err = nil
		}
		return result, err
	}
	reason := fmt.Sprintf("exited with %d", exitErr.ExitCode())
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		reason = "was terminated by uncaught signal " + signalName(ws.Signal())
	}
	return result, message(failureMessage(argv, reason, log.render(c.Color), c.Secrets))
}

func failureMessage(argv []string, reason, output string, secrets []string) string {
	msg := fmt.Sprintf("Failure while executing; `%s` %s.", Redact(ShellJoin(argv), secrets), reason)
	if output == "" {
		return msg
	}
	msg += " Here's the output:\n" + output
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	return msg
}
