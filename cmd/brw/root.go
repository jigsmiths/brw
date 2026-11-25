package main

import (
	"errors"
	"os"
	"regexp"
)

const rootRefusal = `Running Homebrew as root is extremely dangerous and no longer supported.
As Homebrew does not drop privileges on installation you would be giving all
build scripts full access to your system.`

var (
	inContainer = detectContainer
	cgroupJobs  = regexp.MustCompile(`azpl_job|actions_job|docker|garden|kubepods`)
)

// refuseRoot is brew's check-run-command-as-root; `services` is the one command brew lets root run.
func refuseRoot(e *env, cmd string) error {
	if !e.root || cmd == "services" || inContainer() {
		return nil
	}
	return errors.New(rootRefusal) //nolint:staticcheck // brew's own message

}

func detectContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	cgroup, err := os.ReadFile("/proc/1/cgroup")
	return err == nil && cgroupJobs.Match(cgroup)
}
