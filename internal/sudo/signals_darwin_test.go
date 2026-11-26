package sudo

import (
	"syscall"
	"testing"
)

var rubySignals = map[string]syscall.Signal{
	"EXIT": 0, "HUP": 1, "INT": 2, "QUIT": 3, "ILL": 4, "TRAP": 5, "ABRT": 6, "IOT": 6, "EMT": 7, "FPE": 8, "KILL": 9,
	"BUS": 10, "SEGV": 11, "SYS": 12, "PIPE": 13, "ALRM": 14, "TERM": 15, "URG": 16, "STOP": 17, "TSTP": 18, "CONT": 19,
	"CHLD": 20, "CLD": 20, "TTIN": 21, "TTOU": 22, "IO": 23, "XCPU": 24, "XFSZ": 25, "VTALRM": 26, "PROF": 27,
	"WINCH": 28, "USR1": 30, "USR2": 31, "INFO": 29,
}

func TestParseSignalMatchesRubysSignalListOnDarwin(t *testing.T) {
	for name, want := range rubySignals {
		for _, spelled := range []string{name, "SIG" + name} {
			if got, err := ParseSignal(spelled); err != nil || got != want {
				t.Errorf("ParseSignal(%q) = %d, %v, want %d", spelled, got, err, want)
			}
		}
	}
	for _, name := range []string{"PWR", "POLL", "STKFLT"} {
		if _, err := ParseSignal(name); err == nil {
			t.Errorf("ParseSignal(%q) should be unsupported, as in Ruby", name)
		}
	}
}
