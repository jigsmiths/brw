package sudo

import "syscall"

var platformSignalNames = map[syscall.Signal]string{syscall.SIGEMT: "EMT", syscall.SIGINFO: "INFO"}
