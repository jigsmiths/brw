package sudo

import "syscall"

var platformSignalNames = map[syscall.Signal]string{syscall.SIGEMT: "EMT", syscall.SIGINFO: "INFO"}

var signalAliases = map[string]syscall.Signal{"IOT": syscall.SIGABRT, "CLD": syscall.SIGCHLD}
