package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
)

const disclaimHelperEnv = "BRW_TEST_DISCLAIM_HELPER"

func TestDisclaimHelper(t *testing.T) {
	if os.Getenv(disclaimHelperEnv) == "" {
		t.Skip("runs only as the process that execs through ExecDisclaimed")
	}
	err := ExecDisclaimed([]string{"/bin/sh", "-c", "echo disclaimed-ok \"$MARKER\""}, append(os.Environ(), "MARKER=env-passed"))
	t.Fatalf("ExecDisclaimed returned: %v", err)
}

func TestExecDisclaimedReplacesTheProcessAndPassesArgumentsAndEnvironment(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestDisclaimHelper$")
	cmd.Env = append(os.Environ(), disclaimHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "disclaimed-ok env-passed") {
		t.Errorf("err = %v, out = %q", err, out)
	}
}

const missingSymbolHelperEnv = "BRW_TEST_DISCLAIM_SYMBOL_MISSING"

func TestMissingSymbolHelper(t *testing.T) {
	if os.Getenv(missingSymbolHelperEnv) == "" {
		t.Skip("runs only as the process whose lookup of the private symbol fails")
	}
	resolveDisclaim = func() (uintptr, bool) { return 0, false }
	os.Exit(DisclaimMain([]string{"/bin/sh", "-c", "echo fallback-ran"}, os.Stderr))
}

func TestAMissingPrivateSymbolMeansBrewsFallbackNotACrash(t *testing.T) {
	old := resolveDisclaim
	resolveDisclaim = func() (uintptr, bool) { return 0, false }
	t.Cleanup(func() { resolveDisclaim = old })
	if err := ExecDisclaimed([]string{"/bin/sh", "-c", "true"}, nil); !errors.Is(err, ErrDisclaimUnavailable) {
		t.Errorf("err = %v, want ErrDisclaimUnavailable", err)
	}

	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^TestMissingSymbolHelper$")
	cmd.Env = append(os.Environ(), missingSymbolHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("err = %v, out = %q", err, out)
	}
	for _, want := range []string{"Warning: macOS privacy permission isolation is unavailable.", "Continuing with the existing sandbox restrictions.", "fallback-ran"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("output lacks %q: %q", want, out)
		}
	}
}

func TestTheRealSymbolIsFoundAtRuntime(t *testing.T) {
	if fn, ok := resolveDisclaim(); !ok || fn == 0 {
		t.Errorf("resolveDisclaim = %#x, %v on this macOS", fn, ok)
	}
}

func TestExecDisclaimedReportsAMissingProgram(t *testing.T) {
	if err := ExecDisclaimed([]string{"/nonexistent/program"}, nil); err == nil {
		t.Error("exec of a missing program should fail and return")
	}
}
