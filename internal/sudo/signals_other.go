//go:build !darwin && !linux

package sudo

import "syscall"

var platformSignalNames = map[syscall.Signal]string{}
