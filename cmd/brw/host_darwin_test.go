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
