package sandbox

import "testing"

// The values in sys/ttycom.h of the macOS SDK, as the compiler expands _IO, _IOR, _IOW and _IOC.
func TestIoctlRequestsMatchTheSDKHeaders(t *testing.T) {
	for name, tt := range map[string]struct{ got, want uintptr }{
		"TIOCPTYGRANT _IO('t', 84)":                 {tiocPtyGrant, 0x20007454},
		"TIOCPTYUNLK _IO('t', 82)":                  {tiocPtyUnlock, 0x20007452},
		"TIOCPTYGNAME _IOC(IOC_OUT, 't', 83, 128)":  {tiocPtyGName, 0x40807453},
		"TIOCGWINSZ _IOR('t', 104, struct winsize)": {tiocGWinSz, 0x40087468},
		"TIOCSWINSZ _IOW('t', 103, struct winsize)": {tiocSWinSz, 0x80087467},
	} {
		if tt.got != tt.want {
			t.Errorf("%s = %#x, want %#x", name, tt.got, tt.want)
		}
	}
}

func TestTerminalSizeIsKnownOrSaysItIsNot(t *testing.T) {
	old := tputNumber
	tputNumber = func(string) (uint16, bool) { return 0, false }
	t.Cleanup(func() { tputNumber = old })
	if ws, known := sizeFromTput(); known || ws != (winsize{}) {
		t.Errorf("sizeFromTput = %+v, %v, want unknown", ws, known)
	}
	tputNumber = func(capability string) (uint16, bool) {
		if capability == "lines" {
			return 24, true
		}
		return 80, true
	}
	if ws, known := sizeFromTput(); !known || ws.rows != 24 || ws.cols != 80 {
		t.Errorf("sizeFromTput = %+v, %v", ws, known)
	}
}
