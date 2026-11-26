//go:build !darwin

package sandbox

import "os/exec"

func execute(cmd *exec.Cmd, s Spec) error {
	if s.PassStdin {
		cmd.Stdin = s.Stdin
	}
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	return cmd.Run()
}
