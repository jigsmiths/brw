package services

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

type ExitError struct {
	args   []string
	stderr string
	Code   int
}

func (e *ExitError) Error() string {
	return fmt.Sprintf("launchctl %s: %s", strings.Join(e.args, " "), e.stderr)
}

func ExitStatus(err error) (code int, ok bool) {
	if exit, ok := errors.AsType[*ExitError](err); ok {
		return exit.Code, true
	}
	return 0, false
}

type execLaunchctl struct{}

func NewLaunchctl() Launchctl { return execLaunchctl{} }

func (execLaunchctl) run(args ...string) (string, error) {
	var out, errOut bytes.Buffer
	cmd := exec.Command("launchctl", args...)
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return out.String(), &ExitError{args: args, stderr: strings.TrimSpace(errOut.String()), Code: exit.ExitCode()}
	}
	return out.String(), err
}

// A failing print or list means the service is not loaded.
func (l execLaunchctl) query(args ...string) (string, error) {
	out, err := l.run(args...)
	if _, ok := errors.AsType[*ExitError](err); ok {
		return "", ErrNotLoaded
	}
	return out, err
}

func (l execLaunchctl) Print(domain, label string) (string, error) {
	return l.query("print", domain+"/"+label)
}

func (l execLaunchctl) List(label string) (string, error) { return l.query("list", label) }

// Like brew, a nonzero exit still yields stdout; only a launchctl that cannot run is an error.
func (l execLaunchctl) Jobs() (string, error) {
	out, err := l.run("list")
	if _, exited := errors.AsType[*ExitError](err); exited {
		return out, nil
	}
	return out, err
}

func (l execLaunchctl) Enable(domain, label string) error {
	_, err := l.run("enable", domain+"/"+label)
	return err
}

func (l execLaunchctl) Bootstrap(domain, file string) error {
	_, err := l.run("bootstrap", domain, file)
	return err
}

func (l execLaunchctl) Bootout(domain, label string) error {
	_, err := l.run("bootout", domain+"/"+label)
	return err
}

func (l execLaunchctl) Stop(label string) error {
	_, err := l.run("stop", label)
	return err
}
