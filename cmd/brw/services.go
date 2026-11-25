package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/racks"
	"github.com/jigsmiths/brw/internal/rb"
	"github.com/jigsmiths/brw/internal/services"
)

const (
	plistMode     = 0o644
	worldWritable = 0o002
	groupWritable = 0o020
	maxStopWait   = 60
	servicesUsage = "usage: [sudo] brw services [list [--json]] | start|stop|restart [--dry-run] [--sudo-service-user USER] FORMULA [FORMULA ...]"

	ansiBlue   = "\x1b[34m"
	ansiBold   = "\x1b[1m"
	ansiYellow = "\x1b[33m"
	ansiReset  = "\x1b[0m"
)

type serviceTarget struct {
	name      string
	installed bool

	hasService bool
	source     string

	explicit  string
	generated []byte

	prefix       string
	keg          string
	requiresRoot bool
}

func (t serviceTarget) names() []string {
	if t.explicit != "" {
		return []string{t.explicit}
	}
	return services.Names(t.name)
}

func (t serviceTarget) serviceName() string { return t.names()[0] }

func servicesCommand(e *env, args []string) error {
	sub := "list"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	if e.host.OS == "linux" {
		return errors.New("unlike brew, brw services does not support systemd on Linux yet")
	}
	if e.launchctl == nil {
		return errors.New("launchctl is not available")
	}
	switch sub {
	case "list":
		return servicesList(e, args)
	case "start", "stop", "restart":
		return servicesChange(e, sub, args)
	}
	return errors.New(servicesUsage)
}

func (e *env) ohai(format string, a ...any) {
	if e.tty {
		_, _ = fmt.Fprintf(e.out, "%s==>%s %s%s\n", ansiBlue, ansiBold, fmt.Sprintf(format, a...), ansiReset)
		return
	}
	_, _ = fmt.Fprintf(e.out, "==> %s\n", fmt.Sprintf(format, a...))
}

func (e *env) opoo(format string, a ...any) {
	if e.tty {
		_, _ = fmt.Fprintf(e.err, "%sWarning%s: %s\n", ansiYellow, ansiReset, fmt.Sprintf(format, a...))
		return
	}
	_, _ = fmt.Fprintf(e.err, "Warning: %s\n", fmt.Sprintf(format, a...))
}

func (e *env) root() bool { return e.uid == 0 }

func (e *env) destDir() string {
	if e.root() {
		return e.daemonsDir
	}
	return e.agentsDir
}

func (e *env) destsIn(dir string, t serviceTarget) []string {
	var out []string
	for _, n := range t.names() {
		out = append(out, filepath.Join(dir, n+".plist"))
	}
	return out
}

func (e *env) dests(t serviceTarget) []string { return e.destsIn(e.destDir(), t) }

func existing(paths []string) []string {
	return slices.DeleteFunc(paths, func(p string) bool { _, err := os.Stat(p); return err != nil })
}

func (e *env) presentDests(t serviceTarget) []string { return existing(e.dests(t)) }

// registeredInEitherDir is brew's service_file_present?.
func (e *env) registeredInEitherDir(t serviceTarget) bool {
	return len(existing(e.destsIn(e.daemonsDir, t))) > 0 || len(existing(e.destsIn(e.agentsDir, t))) > 0
}

func (e *env) labels(t serviceTarget) []string {
	labels := t.names()
	files := e.presentDests(t)
	if t.hasService {
		files = append(files, t.source)
	}
	for _, f := range files {
		body, err := os.ReadFile(f)
		if label, ok := services.FileLabel(body); err == nil && ok && !slices.Contains(labels, label) {
			labels = append(labels, label)
		}
	}
	return labels
}

type serviceSnap struct {
	loaded      bool
	info        services.Info
	active      string
	loadedNames []string
}

func (e *env) snapshot(t serviceTarget) (serviceSnap, error) {
	s := serviceSnap{active: t.serviceName()}
	for _, label := range e.labels(t) {
		loaded, info, err := services.Query(e.launchctl, services.Domains(e.uid), label)
		if err != nil {
			return s, err
		}
		if !loaded {
			continue
		}
		s.loadedNames = append(s.loadedNames, label)
		if !s.loaded || (info.Running() && !s.info.Running()) {
			s.loaded, s.info, s.active = true, info, label
		}
	}
	return s, nil
}

func (e *env) owner(t serviceTarget, s serviceSnap) *string {
	if registered, ok := e.registered(t, s); ok {
		if body, err := os.ReadFile(registered); err == nil {
			if u, ok := services.FileUser(body); ok {
				return &u
			}
		}
		if filepath.Dir(registered) == e.daemonsDir {
			return new("root")
		}
		return &e.user
	}
	if len(existing(e.destsIn(e.daemonsDir, t))) > 0 {
		return new("root")
	}
	if len(existing(e.destsIn(e.agentsDir, t))) > 0 {
		return &e.user
	}
	return nil
}

// registered is brew's registered_destination, in the directory this run manages.
func (e *env) registered(t serviceTarget, s serviceSnap) (string, bool) {
	present := e.presentDests(t)
	if len(present) == 0 {
		return "", false
	}
	if active := filepath.Join(e.destDir(), s.active+".plist"); slices.Contains(present, active) {
		return active, true
	}
	return present[0], true
}

func (e *env) serviceFile(t serviceTarget, s serviceSnap, prefix string) string {
	if e.registeredInEitherDir(t) {
		if registered, ok := e.registered(t, s); ok {
			return registered
		}
		return e.dests(t)[0]
	}
	return filepath.Join(prefix, "opt", t.name, filepath.Base(t.source))
}

func servicesList(e *env, args []string) error {
	flags := newFlagSet("services list")
	prefixArg := prefixFlag(e, flags)
	asJSON := flags.Bool("json", false, "print JSON")
	sudoUser := flags.String("sudo-service-user", "", "when run as root, run the service as this user")
	rest, err := parse(flags, args)
	if err != nil {
		return errors.Join(err, errors.New(servicesUsage))
	}
	if err := e.setSudoUser(*sudoUser); err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New(servicesUsage)
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return err
	}
	var rows []services.Row
	for _, name := range uniqueNames(kegs) {
		t, ok := e.installedService(prefix, kegs, name)
		if !ok {
			continue
		}
		snap, err := e.snapshot(t)
		if err != nil {
			return err
		}
		timed := false
		if body, err := os.ReadFile(t.source); err == nil {
			timed = services.Timed(body)
		}
		row := services.Row{
			Name:   name,
			Status: services.Classify(services.State{Loaded: snap.loaded, Timed: timed, Info: snap.info}),
			User:   e.owner(t, snap),
			File:   e.serviceFile(t, snap, prefix),
			Loaded: snap.loaded,
		}
		if snap.info.HasExitCode {
			code := snap.info.ExitCode
			row.Exit = &code
		}
		rows = append(rows, row)
	}
	slices.SortFunc(rows, func(a, b services.Row) int { return strings.Compare(a.Name, b.Name) })
	if len(rows) == 0 {
		if *asJSON {
			_, err = fmt.Fprintln(e.out, "[]")
		} else if e.tty {
			e.opoo("No services available to control with `brw services`")
		}
		return err
	}
	if *asJSON {
		out, err := services.FormatJSON(rows)
		if err != nil {
			return err
		}
		_, err = fmt.Fprint(e.out, out)
		return err
	}
	home, _ := os.UserHomeDir()
	_, err = fmt.Fprint(e.out, services.FormatTable(rows, e.tty, home))
	return err
}

func uniqueNames(kegs []cellar.Keg) []string {
	var names []string
	for _, k := range kegs {
		if !slices.Contains(names, k.Name) {
			names = append(names, k.Name)
		}
	}
	return names
}

func (e *env) keptFormula(kegPath, name string) (*rb.Formula, bool) {
	src, err := os.ReadFile(filepath.Join(kegPath, ".brew", name+".rb"))
	if err != nil {
		return nil, false
	}
	tag, err := platform.Tag(e.host)
	if err != nil {
		return nil, false
	}
	f, err := rb.LoadFormula(src, name, tag)
	return f, err == nil
}

func (e *env) generatedService(prefix, kegPath, name string) (plistName string, body []byte, ok bool) {
	f, ok := e.keptFormula(kegPath, name)
	if !ok {
		return "", nil, false
	}
	files, err := f.ServiceFiles(prefix)
	if err != nil {
		return "", nil, false
	}
	for file, contents := range files {
		if base, ok := strings.CutSuffix(file, ".plist"); ok {
			return base, []byte(contents), true
		}
	}
	return "", nil, false
}

func (e *env) serviceRequiresRoot(prefix, kegPath, name string) bool {
	f, ok := e.keptFormula(kegPath, name)
	if !ok {
		return false
	}
	requires, err := f.ServiceRequiresRoot(prefix)
	return err == nil && requires
}

func (e *env) installedService(prefix string, kegs []cellar.Keg, name string) (serviceTarget, bool) {
	var own []cellar.Keg
	for _, k := range kegs {
		if k.Name == name {
			own = append(own, k)
		}
	}
	if len(own) == 0 {
		return serviceTarget{name: name}, false
	}
	keg := cellar.Latest(own).Path
	t := serviceTarget{name: name, installed: true, prefix: prefix, keg: keg, requiresRoot: e.serviceRequiresRoot(prefix, keg, name)}
	if plistName, body, ok := e.generatedService(prefix, keg, name); ok {
		t.generated = body
		if plistName != t.serviceName() {
			t.explicit = plistName
		}
	}
	plist, err := services.FindPlist(keg, t.names())
	t.hasService = err == nil
	if t.hasService {
		t.source = plist
	}
	return t, t.hasService
}

func servicesChange(e *env, verb string, args []string) error {
	flags := newFlagSet("services " + verb)
	var dryRun bool
	prefixArg, journal := changeFlags(e, flags, &dryRun)
	sudoUser := flags.String("sudo-service-user", "", "when run as root, run the service as this user")
	names, err := parse(flags, args)
	if err != nil {
		return err
	}
	if err := e.setSudoUser(*sudoUser); err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("formula(e) missing, please provide a formula name")
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	targets, err := serviceTargets(e, prefix, names)
	if err != nil {
		return err
	}
	if dryRun {
		for _, t := range targets {
			e.planService(verb, t)
		}
		return nil
	}
	switch verb {
	case "start":
		for _, t := range targets {
			if err := e.startService(t, *journal); err != nil {
				return err
			}
		}
	case "stop":
		for _, t := range targets {
			if err := e.stopService(t, *journal); err != nil {
				return err
			}
		}
	default:
		return e.restartServices(targets, *journal)
	}
	return nil
}

func serviceTargets(e *env, prefix string, names []string) ([]serviceTarget, error) {
	kegs, err := e.kegs(prefix)
	if err != nil {
		return nil, err
	}
	index := sync.OnceValues(func() (*api.Index, error) { return e.index(context.Background(), prefix) })
	finder := racks.Finder{Prefix: prefix, Kegs: kegs, Resolve: func(name string) (string, bool) {
		idx, err := index()
		if err != nil {
			return name, true
		}
		f, ok := idx.Lookup(name)
		if !ok {
			return name, false
		}
		return f.Name, true
	}}
	var targets []serviceTarget
	for _, n := range names {
		rack, err := finder.Find(n)
		if missing, ok := errors.AsType[*racks.NoSuchKegError](err); ok {
			targets = append(targets, serviceTarget{name: filepath.Base(missing.Path)})
			continue
		}
		if err != nil {
			return nil, err
		}
		t, _ := e.installedService(prefix, kegs, rack)
		targets = append(targets, t)
	}
	return targets, nil
}

func (e *env) planService(verb string, t serviceTarget) {
	domain := services.Domain(e.uid)
	dests := e.dests(t)
	if verb != "start" {
		_, _ = fmt.Fprintf(e.out, "launchctl bootout %s/%s\nrm %s\n", domain, t.serviceName(), strings.Join(dests, " "))
	}
	if verb != "stop" {
		_, _ = fmt.Fprintf(e.out, "cp %s %s\nlaunchctl enable %s/%s\nlaunchctl bootstrap %s %s\n",
			t.source, dests[0], domain, t.serviceName(), domain, dests[0])
	}
}

func (e *env) reportRunningOrLoaded(t serviceTarget, s serviceSnap, status string) bool {
	running := s.info.Running()
	loadedName := ""
	if running {
		loadedName = s.active
	} else {
		for _, n := range s.loadedNames {
			if n != t.serviceName() {
				loadedName = n
				break
			}
		}
	}
	if loadedName != "" && loadedName != t.serviceName() {
		_, _ = fmt.Fprintf(e.out, "Service `%s` is already loaded as `%s`; a service label migration is pending. "+
			"Use `brw services restart %s` to migrate to `%s`.\n", t.name, loadedName, t.name, t.serviceName())
		return true
	}
	if !running {
		return false
	}
	_, _ = fmt.Fprintf(e.out, "Service `%s` already %s, use `brw services restart %s` to restart.\n", t.name, status, t.name)
	return true
}

func (e *env) startService(t serviceTarget, journal string) error {
	s, err := e.snapshot(t)
	if err != nil {
		return err
	}
	if e.reportRunningOrLoaded(t, s, "started") {
		return nil
	}
	if !t.installed {
		return fmt.Errorf("formula `%s` is not installed", t.name)
	}
	if !t.hasService {
		return fmt.Errorf("formula `%s` has not implemented #plist, #service or provided a locatable service file", t.name)
	}
	body, _, err := e.serviceBody(t)
	if err != nil {
		return err
	}
	if e.sudoUser != "" {
		e.ohai("Setting username in %s to: %s", t.serviceName(), e.sudoUser)
		edited, ok := services.ApplyUserName(body, e.sudoUser)
		if !ok {
			return fmt.Errorf("the service file for `%s` is not an XML plist brw can set a username in", t.name)
		}
		body = edited
	}
	dst := e.dests(t)[0]
	if err := e.replaceServiceFile(t, filepath.Base(dst), body, journal); err != nil {
		return err
	}
	label := t.serviceName()
	if l, ok := services.FileLabel(body); ok {
		label = l
	}
	if err := e.takeRootOwnership(t, dst, body); err != nil {
		return err
	}
	if err := e.checkLoad(t, "start"); err != nil {
		return err
	}
	domain := services.Domain(e.uid)
	if err := e.launchctl.Enable(domain, label); err != nil {
		return err
	}
	if err := e.launchctl.Bootstrap(domain, dst); err != nil {
		return err
	}
	e.ohai("Successfully started `%s` (label: %s)", t.name, label)
	return nil
}

func (e *env) runService(t serviceTarget) error {
	s, err := e.snapshot(t)
	if err != nil {
		return err
	}
	if e.reportRunningOrLoaded(t, s, "running") {
		return nil
	}
	if e.root() {
		_, _ = fmt.Fprintf(e.out, "Service `%s` cannot be run (but can be started) as root.\n", t.name)
		return nil
	}
	body, original, err := e.serviceBody(t)
	if err != nil {
		return err
	}
	if err := e.checkLoad(t, "run"); err != nil {
		return err
	}
	label := t.serviceName()
	if l, ok := services.FileLabel(body); ok {
		label = l
	}
	file := t.source
	if !bytes.Equal(body, original) {
		tmp, err := os.CreateTemp("", t.serviceName()+"-*.plist") //fsx:allow scratch plist for launchctl
		if err != nil {
			return err
		}
		defer func() { _ = os.Remove(tmp.Name()) }() //fsx:allow scratch plist for launchctl
		_, werr := tmp.Write(body)
		if err := errors.Join(werr, tmp.Close()); err != nil {
			return err
		}
		file = tmp.Name()
	}
	if err := e.launchctl.Bootstrap(services.Domain(e.uid), file); err != nil {
		return err
	}
	e.ohai("Successfully ran `%s` (label: %s)", t.name, label)
	return nil
}

func (e *env) serviceBody(t serviceTarget) (body, original []byte, err error) {
	original, err = os.ReadFile(t.source)
	if err != nil {
		return nil, nil, err
	}
	body = original
	if t.generated != nil {
		body = t.generated
	}
	return e.applyUserEnv(t.name, body), original, nil
}

func (e *env) applyUserEnv(name string, body []byte) []byte {
	path := filepath.Join(e.configHome, "services", name+".env")
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() {
		return body
	}
	switch perm := fi.Mode().Perm(); {
	case perm&worldWritable != 0:
		e.opoo("Skipping %s: file is world-writable.", path)
		return body
	case perm&groupWritable != 0:
		e.opoo("Skipping %s: file is group-writable.", path)
		return body
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return body
	}
	vars, skipped := services.ParseEnvFile(string(content))
	for _, line := range skipped {
		e.opoo("Skipping invalid line in %s: %s", path, line)
	}
	out, ok := services.ApplyEnv(body, vars)
	if !ok {
		e.opoo("Skipping %s: the service file is not an XML plist it can edit.", path)
		return body
	}
	return out
}

func (e *env) replaceServiceFile(t serviceTarget, name string, body []byte, journal string) error {
	if err := os.MkdirAll(e.destDir(), 0o755); err != nil { //fsx:allow launch agents or daemons dir
		return err
	}
	return withRoot(e.destDir(), journal, func(root *fsx.Root) error {
		for _, p := range e.presentDests(t) {
			if err := root.Remove(filepath.Base(p)); err != nil {
				return err
			}
		}
		return root.WriteFile(name, body, plistMode)
	})
}

func (e *env) removeServiceFiles(t serviceTarget, journal string) error {
	present := e.presentDests(t)
	if len(present) == 0 {
		return nil
	}
	return withRoot(e.destDir(), journal, func(root *fsx.Root) error {
		for _, p := range present {
			if err := root.Remove(filepath.Base(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
		return nil
	})
}

func (e *env) sleepFor(d time.Duration) {
	if e.sleep != nil {
		e.sleep(d)
		return
	}
	time.Sleep(d)
}

func (e *env) running(label string) (bool, error) {
	loaded, _, err := services.Query(e.launchctl, services.Domains(e.uid), label)
	return loaded, err
}

func (e *env) stopService(t serviceTarget, journal string) error {
	s, err := e.snapshot(t)
	if err != nil {
		return err
	}
	if !s.loaded {
		if err := e.removeServiceFiles(t, journal); err != nil {
			return err
		}
		if e.registeredInEitherDir(t) {
			sudo := "sudo "
			if e.root() {
				sudo = ""
			}
			return fmt.Errorf("service `%s` is started as `%s`. Try:\n  %sbrw services stop %s", t.name, *e.owner(t, s), sudo, t.name)
		}
		for _, n := range t.names() {
			if e.launchctl.Bootout(services.Domain(e.uid), n) == nil {
				e.ohai("Successfully stopped `%s` (label: %s)", t.name, n)
				return nil
			}
		}
		e.opoo("Service `%s` is not started.", t.name)
		return nil
	}
	_, _ = fmt.Fprintf(e.out, "Stopping `%s`... (might take a while)\n", t.name)
	for _, label := range s.loadedNames {
		if err := e.bootoutAll(label); err != nil {
			return err
		}
	}
	still := false
	for _, label := range s.loadedNames {
		up, err := e.running(label)
		if err != nil {
			return err
		}
		still = still || up
	}
	if !still {
		if err := e.removeServiceFiles(t, journal); err != nil {
			return err
		}
	}
	labels := strings.Join(s.loadedNames, ", ")
	if still {
		e.opoo("Unable to stop `%s` (label: %s)", t.name, labels)
		return nil
	}
	e.ohai("Successfully stopped `%s` (label: %s)", t.name, labels)
	return nil
}

func (e *env) bootoutAll(label string) error {
	for _, domain := range services.Domains(e.uid) {
		up, err := e.running(label)
		if err != nil || !up {
			return err
		}
		code, err := e.bootout(domain, label)
		if err != nil {
			return err
		}
		for range maxStopWait {
			if code == services.StatusESRCH || code == services.StatusDomainNotSupported {
				break
			}
			up, err := e.running(label)
			if err != nil {
				return err
			}
			if code != services.StatusEINPROGRESS && !up {
				break
			}
			e.sleepFor(time.Second)
			if code, err = e.bootout(domain, label); err != nil {
				return err
			}
		}
		if up, err := e.running(label); err != nil {
			return err
		} else if up {
			_ = e.launchctl.Stop(label)
		}
	}
	return nil
}

func (e *env) bootout(domain, label string) (int, error) {
	err := e.launchctl.Bootout(domain, label)
	if err == nil {
		return 0, nil
	}
	if code, ok := services.ExitStatus(err); ok {
		return code, nil
	}
	return 0, err
}

func (e *env) restartServices(targets []serviceTarget, journal string) error {
	var ran, started []serviceTarget
	for _, t := range targets {
		s, err := e.snapshot(t)
		if err != nil {
			return err
		}
		if s.loaded && len(e.presentDests(t)) == 0 {
			ran = append(ran, t)
		} else {
			started = append(started, t)
		}
		if s.loaded {
			if err := e.stopService(t, journal); err != nil {
				return err
			}
		}
	}
	for _, t := range ran {
		if err := e.runService(t); err != nil {
			return err
		}
	}
	for _, t := range started {
		if err := e.startService(t, journal); err != nil {
			return err
		}
	}
	return nil
}
