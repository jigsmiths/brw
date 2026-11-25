package main

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/sudo"
)

func TestFindServiceAsTheUserUsesBrewsDomains(t *testing.T) {
	clearDomainEnv(t)
	e, _, _, lc := servicesEnv(t)
	lc.loaded["com.example.agent"] = runningOut
	for label, want := range map[string]bool{"com.example.agent": true, "com.example.none": false} {
		if got, err := e.findService(context.Background(), label, sudo.User); err != nil || got != want {
			t.Errorf("%s: found = %v, %v, want %v", label, got, err, want)
		}
	}
}

func TestFindServiceAsRootRunsLaunchctlThroughSudo(t *testing.T) {
	clearDomainEnv(t)
	e, _, _, _ := servicesEnv(t)
	var runs []sudo.Command
	e.sudoRun = func(_ context.Context, c sudo.Command) (sudo.Result, error) {
		runs = append(runs, c)
		if c.Args[0] == "list" {
			return sudo.Result{Stdout: "{ \"Label\" = \"com.example.daemon\"; };\n"}, nil
		}
		return sudo.Result{}, errors.New("Failure while executing")
	}
	found, err := e.findService(context.Background(), "com.example.daemon", sudo.Root)
	if err != nil || !found {
		t.Fatalf("found = %v, %v", found, err)
	}
	var got [][]string
	for _, c := range runs {
		if c.Exe != "/bin/launchctl" || c.Privilege != sudo.Root || c.Stderr != nil {
			t.Errorf("run = %v", c)
		}
		got = append(got, c.Args)
	}
	want := [][]string{{"print", "gui/501/com.example.daemon"}, {"print", "user/501/com.example.daemon"}, {"list", "com.example.daemon"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("args = %q, want %q", got, want)
	}
}

func TestFindServicePrintsBrewsDomainWarningOnce(t *testing.T) {
	clearDomainEnv(t)
	t.Setenv("HOMEBREW_SUDO_USER", "me")
	e, _, _, _ := servicesEnv(t)
	for range 2 {
		if _, err := e.findService(context.Background(), "com.example.agent", sudo.User); err != nil {
			t.Fatal(err)
		}
	}
	if want := sudoDomainWarning + domainHints; stderr(e) != want {
		t.Errorf("stderr = %q, want %q", stderr(e), want)
	}
}

func TestFindServiceEchoesLikeOdebugUnderDebug(t *testing.T) {
	clearDomainEnv(t)
	t.Setenv("HOMEBREW_NO_COLOR", "1")
	e, _, _, lc := servicesEnv(t)
	lc.loaded["com.example.agent"] = runningOut
	e.debug = true
	if _, err := e.findService(context.Background(), "com.example.agent", sudo.User); err != nil {
		t.Fatal(err)
	}
	if want := "==> /bin/launchctl print gui/501/com.example.agent\n" + runningOut; !strings.HasSuffix(stderr(e), want) {
		t.Errorf("stderr = %q, want it to end with %q", stderr(e), want)
	}
}
