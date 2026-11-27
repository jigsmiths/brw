package poststep

import (
	"io"
	"os"
	"runtime"
)

// Host is the machine the steps configure. Every field is a real value; the
// runner never looks at the running system itself.
type Host struct {
	Home       string
	Temp       string
	MacVersion string // product version, darwin only
	Kernel     string // uname -r
	GOARCH     string
}

func OrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

const (
	darwinTemp = "/private/tmp"
	linuxTemp  = "/tmp"
)

// CurrentHost reads the running system once; home and macVersion come from the caller, which knows them.
func CurrentHost(goos, home, macVersion string) Host {
	temp := os.Getenv("HOMEBREW_TEMP")
	switch {
	case temp != "":
	case goos == "darwin":
		temp = darwinTemp
	default:
		temp = linuxTemp
	}
	return Host{Home: home, Temp: temp, MacVersion: macVersion, Kernel: kernelRelease(), GOARCH: runtime.GOARCH}
}
