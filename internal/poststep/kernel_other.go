//go:build !darwin

package poststep

import (
	"os"
	"strings"
)

func kernelRelease() string {
	b, _ := os.ReadFile("/proc/sys/kernel/osrelease")
	return strings.TrimSpace(string(b))
}
