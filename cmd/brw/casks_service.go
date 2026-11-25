package main

import (
	"context"
	"os"

	"github.com/jigsmiths/brw/internal/services"
	"github.com/jigsmiths/brw/internal/sudo"
)

// rootLaunchctl probes launchd as root, as launchctl_run does with sudo: true.
type rootLaunchctl struct {
	services.Launchctl
	run sudo.Runner
}

func (r rootLaunchctl) query(args ...string) (string, error) {
	c := sudo.Command{Exe: launchctlPath, Args: args, Privilege: sudo.Root, MustSucceed: true, Secrets: sudo.SensitiveValues()}
	res, err := r.run(context.Background(), c)
	if err != nil {
		return "", services.ErrNotLoaded
	}
	return res.Stdout, nil
}

func (r rootLaunchctl) Print(domain, label string) (string, error) {
	return r.query("print", domain+"/"+label)
}

func (r rootLaunchctl) List(label string) (string, error) { return r.query("list", label) }

func (e *env) setTracing(t tracing) {
	e.verbose, e.debug = t.verbose, t.debug || os.Getenv("HOMEBREW_DEBUG") != ""
}

func (e *env) runner() sudo.Runner {
	if e.sudoRun != nil {
		return e.sudoRun
	}
	return sudo.Exec
}

// findService is Homebrew::Services::System.launchctl_find_service.
func (e *env) findService(_ context.Context, label string, privilege sudo.Privilege) (bool, error) {
	domains, err := e.domains()
	if err != nil {
		return false, err
	}
	l := e.launchctl
	if privilege != sudo.User {
		l = rootLaunchctl{Launchctl: l, run: e.runner()}
	}
	if e.debug {
		l = debugLaunchctl{Launchctl: l, e: e}
	}
	loaded, _, err := services.Query(l, domains, label)
	return loaded, err
}
