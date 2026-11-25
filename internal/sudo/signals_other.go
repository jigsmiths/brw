//go:build !darwin && !linux

package sudo

import "syscall"

var platformSignalNames = map[syscall.Signal]string{}

var signalAliases = map[string]syscall.Signal{}
