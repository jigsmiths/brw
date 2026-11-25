package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/jigsmiths/brw/internal/services"
)

const (
	defaultMaxWait = 60.0
	servicesUsage  = "usage: [sudo] brw services [list|run|start|stop|restart|kill|info|cleanup] [FORMULA ...|--all]"

	optAll     = "all"
	optDryRun  = "dry-run"
	optFile    = "file"
	optJSON    = "json"
	optKeep    = "keep"
	optMaxWait = "max-wait"
	optNoWait  = "no-wait"
	optVerbose = "verbose"
	kindFlag   = "flag"
	kindSwitch = "switch"
)

type serviceSub struct {
	name    string
	aliases []string
	noNamed bool
	options []string
}

var serviceSubs = []serviceSub{
	{name: "list", aliases: []string{"ls"}, noNamed: true, options: []string{optJSON}},
	{name: "run", options: []string{optFile, optAll}},
	{name: "start", aliases: []string{"launch", "load", "s", "l"}, options: []string{optFile, optAll, optDryRun}},
	{name: "stop", aliases: []string{"unload", "terminate", "term", "t", "u"}, options: []string{optMaxWait, optNoWait, optKeep, optAll, optDryRun}},
	{name: "restart", aliases: []string{"relaunch", "reload", "r"}, options: []string{optFile, optAll, optDryRun}},
	{name: "kill", aliases: []string{"k"}, options: []string{optAll}},
	{name: "info", aliases: []string{"i"}, options: []string{optAll, optJSON}},
	{name: "cleanup", aliases: []string{"clean", "cl", "rm"}, noNamed: true},
}

func findServiceSub(name string) (serviceSub, bool) {
	for _, s := range serviceSubs {
		if s.name == name || slices.Contains(s.aliases, name) {
			return s, true
		}
	}
	return serviceSub{}, false
}

type servicesOpts struct {
	prefix, journal, file, sudoUser                 string
	dryRun, all, json, keep, noWait, verbose, debug bool
	maxWait                                         float64
	names                                           []string
}

func parseServices(e *env, args []string) (serviceSub, *servicesOpts, error) {
	flags := newFlagSet("services")
	o := &servicesOpts{}
	flags.StringVar(&o.prefix, "prefix", defaultPrefix(e.host), "Homebrew prefix")
	flags.StringVar(&o.journal, "journal", "", "append changes to this file")
	flags.StringVar(&o.sudoUser, "sudo-service-user", "", "when run as root, run the service as this user")
	flags.StringVar(&o.file, optFile, "", "use the service file from this location")
	maxWait := flags.String(optMaxWait, "", "wait at most this many seconds for stop")
	for _, n := range []string{optDryRun, "n"} {
		flags.BoolVar(&o.dryRun, n, false, "show what would change")
	}
	for _, n := range []string{optVerbose, "v"} {
		flags.BoolVar(&o.verbose, n, false, "show more")
	}
	for _, n := range []string{"debug", "d"} {
		flags.BoolVar(&o.debug, n, false, "show debugging information")
	}
	flags.BoolVar(&o.all, optAll, false, "act on all services")
	flags.BoolVar(&o.json, optJSON, false, "print JSON")
	flags.BoolVar(&o.keep, optKeep, false, "do not unregister the service")
	flags.BoolVar(&o.noWait, optNoWait, false, "do not wait for the service to stop")
	names, err := parse(flags, args)
	if err != nil {
		return serviceSub{}, nil, errors.Join(err, errors.New(servicesUsage))
	}
	passed := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { passed[canonicalOption(f.Name)] = true })
	o.maxWait = defaultMaxWait
	if passed[optMaxWait] {
		o.maxWait = services.ToF(*maxWait)
	}
	if passed[optAll] && passed[optFile] {
		return serviceSub{}, nil, errors.New("Options --all and --file are mutually exclusive.") //nolint:staticcheck // matches brew's error text
	}
	if passed[optMaxWait] && passed[optNoWait] {
		return serviceSub{}, nil, errors.New("Options --max-wait and --no-wait are mutually exclusive.") //nolint:staticcheck // matches brew's error text
	}
	name := "list"
	if len(names) > 0 {
		name, names = names[0], names[1:]
	}
	sub, ok := findServiceSub(name)
	if !ok {
		return serviceSub{}, nil, fmt.Errorf("unknown subcommand: `%s`", name)
	}
	if sub.noNamed && len(names) > 0 {
		return serviceSub{}, nil, errors.New("This command does not take named arguments.") //nolint:staticcheck // matches brew's error text
	}
	for _, opt := range []string{optAll, optDryRun, optFile, optJSON, optKeep, optMaxWait, optNoWait} {
		if !passed[opt] || slices.Contains(sub.options, opt) {
			continue
		}
		kind := kindSwitch
		if opt == optFile || opt == optMaxWait {
			kind = kindFlag
		}
		return serviceSub{}, nil, fmt.Errorf("The `%s` subcommand does not accept the `--%s` %s.", sub.name, opt, kind) //nolint:staticcheck // matches brew's error text
	}
	o.names = names
	return sub, o, nil
}

func canonicalOption(name string) string {
	switch name {
	case "n":
		return optDryRun
	case "v":
		return optVerbose
	}
	return name
}

func servicesCommand(e *env, args []string) error {
	sub, o, err := parseServices(e, args)
	if err != nil {
		return err
	}
	if _, tmux := os.LookupEnv("HOMEBREW_TMUX"); tmux && e.clipboardBroken != nil && e.clipboardBroken() {
		return errors.New("`brw services` cannot run under tmux!") //nolint:staticcheck // matches brew's error text
	}
	if e.host.OS == "linux" {
		return errors.New("unlike brew, brw services does not support systemd on Linux yet")
	}
	if e.launchctl == nil {
		return errors.New("launchctl is not available")
	}
	e.verbose = o.verbose
	e.debug = o.debug || os.Getenv("HOMEBREW_DEBUG") != ""
	if e.debug {
		e.launchctl = debugLaunchctl{Launchctl: e.launchctl, e: e}
	}
	e.targetKnown = false
	if err := e.setSudoUser(o.sudoUser); err != nil {
		return err
	}
	prefix, err := filepath.Abs(o.prefix)
	if err != nil {
		return err
	}
	switch sub.name {
	case "list":
		return servicesList(e, o, prefix)
	case "cleanup":
		return e.servicesCleanup(o, prefix)
	}
	if len(o.names) > 0 && o.all {
		e.opoo("The `--all` argument overrides provided formula argument!")
	}
	targets, err := e.serviceTargetsFor(sub.name, o, prefix)
	if err != nil {
		return err
	}
	if o.all && len(targets) == 0 {
		return nil
	}
	if len(targets) == 0 {
		return errors.New("Formula(e) missing, please provide a formula name or use `--all`.") //nolint:staticcheck // matches brew's error text
	}
	switch sub.name {
	case "run":
		return e.servicesRun(targets, o)
	case "kill":
		return e.servicesKill(targets)
	case "info":
		return e.servicesInfo(targets, o, prefix)
	}
	return e.servicesChange(sub.name, targets, o)
}

func (e *env) serviceTargetsFor(sub string, o *servicesOpts, prefix string) ([]serviceTarget, error) {
	if o.all {
		return e.allServiceTargets(sub, prefix)
	}
	if len(o.names) == 0 {
		return nil, nil
	}
	return serviceTargets(e, prefix, o.names)
}

func (e *env) allServiceTargets(sub, prefix string) ([]serviceTarget, error) {
	kegs, err := e.kegs(prefix)
	if err != nil {
		return nil, err
	}
	names := uniqueNames(kegs)
	slices.Sort(names)
	var out []serviceTarget
	for _, name := range names {
		t, ok := e.installedService(prefix, kegs, name)
		if !ok || !t.block.Defined {
			continue
		}
		if sub == "start" || sub == "stop" {
			snap, err := e.snapshot(t)
			if err != nil {
				return nil, err
			}
			if snap.loaded != (sub == "stop") {
				continue
			}
			if !e.root() {
				if owner := e.owner(t, snap); owner != nil && *owner == "root" {
					continue
				}
			}
		}
		out = append(out, t)
	}
	return out, nil
}

func (e *env) servicesChange(verb string, targets []serviceTarget, o *servicesOpts) error {
	if o.dryRun {
		for _, t := range targets {
			if err := e.planService(verb, t); err != nil {
				return err
			}
		}
		return nil
	}
	switch verb {
	case "start":
		for _, t := range targets {
			if err := e.startService(t, o); err != nil {
				return err
			}
		}
	case "stop":
		for _, t := range targets {
			if err := e.stopService(t, o); err != nil {
				return err
			}
		}
	default:
		return e.restartServices(targets, o)
	}
	return nil
}

func (e *env) servicesRun(targets []serviceTarget, o *servicesOpts) error {
	for _, t := range targets {
		if err := e.runService(t, o.file); err != nil {
			return err
		}
	}
	return nil
}

func checkServiceFile(file string) error {
	if file == "" {
		return nil
	}
	if _, err := os.Stat(file); err != nil {
		return errors.New("Provided service file does not exist.") //nolint:staticcheck // matches brew's error text
	}
	return nil
}
