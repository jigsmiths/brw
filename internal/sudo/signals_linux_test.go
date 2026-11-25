package sudo

import (
	"syscall"
	"testing"
)

func TestParseSignalKnowsLinuxAliases(t *testing.T) {
	for name, want := range map[string]syscall.Signal{
		"PWR": syscall.SIGPWR, "IOT": syscall.SIGABRT, "CLD": syscall.SIGCHLD, "POLL": syscall.SIGIO,
	} {
		if got, err := ParseSignal(name); err != nil || got != want {
			t.Errorf("ParseSignal(%q) = %d, %v, want %d", name, got, err, want)
		}
	}
}
