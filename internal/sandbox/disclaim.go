package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

// DisclaimCommand is the hidden subcommand that runs a command with macOS responsibility disclaimed, as brew's utils/disclaim.rb does.
const DisclaimCommand = "__disclaim-exec"

// ErrDisclaimUnavailable means macOS would not let this process disclaim responsibility for the child.
var ErrDisclaimUnavailable = errors.New("macOS privacy permission isolation is unavailable")

const disclaimFailureExit = 1

// DisclaimMain execs args disclaimed; when macOS cannot, it says so and execs them as they are, which is brew's fallback.
func DisclaimMain(args []string, stderr io.Writer) int {
	err := ExecDisclaimed(args, os.Environ())
	if !errors.Is(err, ErrDisclaimUnavailable) {
		_, _ = fmt.Fprintf(stderr, "brw: %v\n", err)
		return disclaimFailureExit
	}
	_, _ = fmt.Fprintf(stderr, "Warning: %s.\nContinuing with the existing sandbox restrictions.\n", err)
	err = syscall.Exec(args[0], args, os.Environ())
	_, _ = fmt.Fprintf(stderr, "brw: %v\n", err)
	return disclaimFailureExit
}
