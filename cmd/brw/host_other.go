//go:build !darwin

package main

import (
	"runtime"

	"github.com/jigsmiths/brw/internal/cellar"
)

func macVersion() string { return "" }

func buildSystem() cellar.BuiltOn { return cellar.BuiltOn{OS: runtime.GOOS} }

func sdkPath() string { return "" }
