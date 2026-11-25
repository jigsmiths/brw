package main

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

const (
	sudoDomainWarning = "Warning: running through sudo, using user/* instead of gui/* domain!\n"
	domainHints       = "Hide this warning by setting `HOMEBREW_SERVICES_NO_DOMAIN_WARNING=1`.\n" +
		"Hide these hints with `HOMEBREW_NO_ENV_HINTS=1` (see `man brew`).\n"
)

func clearDomainEnv(t *testing.T) {
	t.Helper()
	for _, v := range []string{"HOMEBREW_SUDO_USER", "HOMEBREW_SSH_TTY", "HOMEBREW_SERVICES_NO_DOMAIN_WARNING", "HOMEBREW_NO_ENV_HINTS"} {
		t.Setenv(v, "")
	}
}

func TestServicesUseTheUserDomainThroughSudo(t *testing.T) {
	clearDomainEnv(t)
	t.Setenv("HOMEBREW_SUDO_USER", "me")
	e, prefix, agents, lc := servicesEnv(t)
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	if want := "enable user/501/sh.brew.foo\nbootstrap user/501 " + dst; calls(lc) != want {
		t.Errorf("calls = %q, want %q", calls(lc), want)
	}
	if want := sudoDomainWarning + domainHints; stderr(e) != want {
		t.Errorf("stderr = %q, want %q (once per run)", stderr(e), want)
	}
}

func TestServicesDomainWarningCanBeHidden(t *testing.T) {
	clearDomainEnv(t)
	t.Setenv("HOMEBREW_SUDO_USER", "me")
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	e, prefix, _, _ := servicesEnv(t)
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if stderr(e) != sudoDomainWarning {
		t.Errorf("hints hidden: %q", stderr(e))
	}

	t.Setenv("HOMEBREW_SERVICES_NO_DOMAIN_WARNING", "1")
	e, prefix, _, lc := servicesEnv(t)
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if stderr(e) != "" || !strings.HasPrefix(calls(lc), "enable user/501/") {
		t.Errorf("stderr = %q, calls = %q", stderr(e), calls(lc))
	}
}

func TestServicesFailWhenTheConsoleCannotBeStatedOverSsh(t *testing.T) {
	clearDomainEnv(t)
	t.Setenv("HOMEBREW_SSH_TTY", "/dev/ttys001")
	e, prefix, _, lc := servicesEnv(t)
	e.consoleUID = func() (int, error) { return 0, errors.New("stat /dev/console: no such file or directory") }
	_, err := runCmd(t, e, "services", "list", "-prefix", prefix)
	if err == nil || !strings.Contains(err.Error(), "/dev/console") || len(lc.calls) != 0 {
		t.Errorf("err = %v, calls = %q", err, lc.calls)
	}
}

func TestServicesUseTheUserDomainOverSshWithoutConsoleOwnership(t *testing.T) {
	clearDomainEnv(t)
	t.Setenv("HOMEBREW_SSH_TTY", "/dev/ttys001")
	e, prefix, _, lc := servicesEnv(t)
	e.consoleUID = func() (int, error) { return 0, nil }
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	want := "Warning: running over SSH without /dev/console ownership, using user/* instead of gui/* domain!\n" + domainHints
	if stderr(e) != want || !strings.HasPrefix(calls(lc), "enable user/501/") {
		t.Errorf("stderr = %q, calls = %q", stderr(e), calls(lc))
	}

	e, prefix, _, lc = servicesEnv(t)
	e.consoleUID = func() (int, error) { return 501, nil }
	if _, err := runCmd(t, e, "services", "start", "-prefix", prefix, "foo"); err != nil {
		t.Fatal(err)
	}
	if stderr(e) != "" || !strings.HasPrefix(calls(lc), "enable gui/501/") {
		t.Errorf("owning the console keeps gui: stderr = %q, calls = %q", stderr(e), calls(lc))
	}
}
