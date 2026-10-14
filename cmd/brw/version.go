package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"

	"github.com/jigsmiths/brw/internal/selfupdate"
)

// stampedVersion is set by releases: -ldflags "-X main.stampedVersion=1.2.3".
var stampedVersion string

// buildInfo returns the version brw was built as, empty for a local build, and
// the commit it was built from when known.
func buildInfo() (v, revision string) {
	module := ""
	if info, ok := debug.ReadBuildInfo(); ok {
		module = info.Main.Version
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				revision = setting.Value
			}
		}
	}
	return buildVersion(stampedVersion, module), revision
}

// buildVersion prefers the stamped version, then the module version of go install.
func buildVersion(stamped, module string) string {
	if stamped != "" {
		return stamped
	}
	if module != "(devel)" {
		return module
	}
	return ""
}

func versionLine(version, revision string) string {
	switch {
	case version != "":
		return "brw " + version + "\n"
	case len(revision) >= 7:
		return "brw dev (" + revision[:7] + ")\n"
	}
	return "brw dev\n"
}

func versionCommand(e *env, args []string) error {
	fs := newFlagSet("version")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("usage: brw version")
	}
	_, err = fmt.Fprint(e.out, versionLine(buildInfo()))
	return err
}

// selfUpdateBrw replaces the running binary with the latest release.
func selfUpdateBrw(ctx context.Context) (selfupdate.Result, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe) // replace the file, not a link to it
	}
	if err != nil {
		return selfupdate.Result{}, err
	}
	u := &selfupdate.Updater{Exe: exe, Current: stampedVersion, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
	return u.Run(ctx)
}
