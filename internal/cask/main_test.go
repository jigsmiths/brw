package cask

import (
	"os"
	"testing"
)

// Marks sudo as already checked so no test probes the host's sudo.
func TestMain(m *testing.M) {
	for _, k := range []string{"HOMEBREW_NO_SUDO", "HOMEBREW_DEBUG", "HOMEBREW_VERBOSE", "HOMEBREW_LIBRARY"} {
		_ = os.Unsetenv(k)
	}
	_ = os.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	os.Exit(m.Run())
}
