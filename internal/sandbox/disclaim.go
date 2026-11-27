package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
)

const DisclaimCommand = "__disclaim-exec"

var ErrDisclaimUnavailable = errors.New("macOS privacy permission isolation is unavailable")

const disclaimFailureExit = 1

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
