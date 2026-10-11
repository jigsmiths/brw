package main

import "syscall"

// macVersion is the macOS product version, such as "27.0.1". Reading it from
// the kernel avoids running sw_vers, which costs about 16ms.
func macVersion() string {
	v, err := syscall.Sysctl("kern.osproductversion")
	if err != nil {
		return ""
	}
	return v
}
