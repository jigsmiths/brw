package main

import (
	"os/exec"
	"strings"
	"testing"
)

func TestMacVersionMatchesSwVers(t *testing.T) {
	want, err := exec.Command("sw_vers", "-productVersion").Output()
	if err != nil {
		t.Skip("sw_vers is not available")
	}
	if got := macVersion(); got != strings.TrimSpace(string(want)) {
		t.Errorf("macVersion() = %q, sw_vers says %q", got, want)
	}
}

func TestBuildSystemDescribesThisMac(t *testing.T) {
	on := buildSystem()
	if on.OS != "Macintosh" || !strings.HasPrefix(on.OSVersion, "macOS ") || strings.Count(on.OSVersion, ".") > 1 {
		t.Errorf("os: %+v", on)
	}
	if on.CPUFamily == "" || (on.PreferredPerl != "5.34" && on.PreferredPerl != "5.30") {
		t.Errorf("cpu and perl: %+v", on)
	}
	t.Logf("%+v", on)
}
