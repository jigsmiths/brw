//go:build !darwin

package sandbox

// ExecDisclaimed has no meaning off macOS, where the sandbox never runs.
func ExecDisclaimed(_, _ []string) error { return ErrDisclaimUnavailable }
