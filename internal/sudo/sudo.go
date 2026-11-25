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
	Exe  string
	Args []string
	// Env holds NAME=value entries; with sudo they are passed through `sudo -E`.
	Env       []string
	Input     string
	Privilege Privilege
	// MustSucceed makes a non-zero exit an error.
	MustSucceed bool
	// Stdout and Stderr receive redacted lines as they arrive; nil discards that stream.
	Stdout, Stderr io.Writer
	// Trace receives the redacted command line before it runs.
	Trace io.Writer
	// Color paints stderr lines in a failure message red.
	Color   bool
	Secrets []string
}

type Result struct{ Stdout, Stderr string }

// Runner is injectable so callers can test without running anything.
type Runner func(ctx context.Context, c Command) (Result, error)

type message string

func (m message) Error() string { return string(m) }

// Argv is the command line to execute, including the sudo or env prefix.
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

// Exec runs c, first checking that sudo is usable when c needs it.
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
