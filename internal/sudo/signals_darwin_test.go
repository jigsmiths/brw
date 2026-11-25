package sudo

import (
	"syscall"
	"testing"
)

func TestParseSignalKnowsDarwinNames(t *testing.T) {
	for name, want := range map[string]syscall.Signal{
		"EMT": syscall.SIGEMT, "INFO": syscall.SIGINFO, "SIGINFO": syscall.SIGINFO, "IOT": syscall.SIGABRT, "CLD": syscall.SIGCHLD,
	} {
		if got, err := ParseSignal(name); err != nil || got != want {
			t.Errorf("ParseSignal(%q) = %d, %v, want %d", name, got, err, want)
		}
	}
}
