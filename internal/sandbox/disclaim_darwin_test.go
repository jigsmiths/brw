package sandbox

import (
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

func TestExecDisclaimedReportsAMissingProgram(t *testing.T) {
	if err := ExecDisclaimed([]string{"/nonexistent/program"}, nil); err == nil {
		t.Error("exec of a missing program should fail and return")
	}
}
