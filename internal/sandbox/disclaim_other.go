//go:build !darwin

package sandbox

func ExecDisclaimed(_, _ []string) error { return ErrDisclaimUnavailable }
