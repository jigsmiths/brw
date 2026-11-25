//go:build !darwin && !linux

package cask

import "syscall"

var platformSignalNames = map[syscall.Signal]string{}
