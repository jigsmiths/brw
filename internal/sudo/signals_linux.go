package sudo

import "syscall"

var platformSignalNames = map[syscall.Signal]string{syscall.SIGPWR: "PWR"}
