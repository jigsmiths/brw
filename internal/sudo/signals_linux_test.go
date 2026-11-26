package sudo

import (
	"syscall"
	"testing"
)

var rubySignals = map[string]syscall.Signal{
	"EXIT": 0, "HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "IOT": 6, "BUS": 7, "FPE": 8, "KILL": 9,
	"USR1": 10, "SEGV": 11, "USR2": 12, "PIPE": 13, "ALRM": 14, "TERM": 15, "CHLD": 17, "CLD": 17, "CONT": 18, "STOP": 19,
	"TSTP": 20, "TTIN": 21, "TTOU": 22, "URG": 23, "XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27, "WINCH": 28,
	"IO": 29, "POLL": 29, "PWR": 30, "SYS": 31,
}

func TestParseSignalMatchesRubysSignalListOnLinux(t *testing.T) {
	for name, want := range rubySignals {
		for _, spelled := range []string{name, "SIG" + name} {
			if got, err := ParseSignal(spelled); err != nil || got != want {
				t.Errorf("ParseSignal(%q) = %d, %v, want %d", spelled, got, err, want)
			}
		}
	}
	for _, name := range []string{"STKFLT", "EMT", "INFO", "RTMIN", "UNUSED"} {
		if _, err := ParseSignal(name); err == nil {
			t.Errorf("ParseSignal(%q) should be unsupported, as in Ruby", name)
		}
	}
}
