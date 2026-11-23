//go:build !darwin

package main

import (
	"runtime"

	"github.com/jigsmiths/brw/internal/cellar"
)

func macVersion() string { return "" }

// buildSystem is what a receipt records about the machine a formula was
// built on; brw builds formulae only on macOS so far.
func buildSystem() cellar.BuiltOn { return cellar.BuiltOn{OS: runtime.GOOS} }

func sdkPath() string { return "" }
