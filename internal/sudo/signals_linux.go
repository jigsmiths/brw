package sudo

import "syscall"

var platformSignalNames = map[syscall.Signal]string{syscall.SIGPWR: "PWR"}

var signalAliases = map[string]syscall.Signal{"IOT": syscall.SIGABRT, "CLD": syscall.SIGCHLD, "POLL": syscall.SIGIO}
