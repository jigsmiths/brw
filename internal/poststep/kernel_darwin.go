package poststep

import "syscall"

func kernelRelease() string {
	release, _ := syscall.Sysctl("kern.osrelease")
	return release
}
