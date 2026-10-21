package main

import (
	"os"
	"syscall"
	"unsafe"
)

// terminalWidth is the number of columns f has, or 0 if it cannot be told.
func terminalWidth(f *os.File) int {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 {
		return 0
	}
	return int(ws.Col)
}
