package cask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	signalPause = 3 * time.Second
	eperm       = "Operation not permitted"
)

var signals = map[string]syscall.Signal{
	"HUP": syscall.SIGHUP, "INT": syscall.SIGINT, "QUIT": syscall.SIGQUIT, "ILL": syscall.SIGILL,
	"TRAP": syscall.SIGTRAP, "ABRT": syscall.SIGABRT, "FPE": syscall.SIGFPE, "KILL": syscall.SIGKILL,
	"BUS": syscall.SIGBUS, "SEGV": syscall.SIGSEGV, "SYS": syscall.SIGSYS, "PIPE": syscall.SIGPIPE,
	"ALRM": syscall.SIGALRM, "TERM": syscall.SIGTERM, "URG": syscall.SIGURG, "STOP": syscall.SIGSTOP,
	"TSTP": syscall.SIGTSTP, "CONT": syscall.SIGCONT, "CHLD": syscall.SIGCHLD, "TTIN": syscall.SIGTTIN,
	"TTOU": syscall.SIGTTOU, "IO": syscall.SIGIO, "XCPU": syscall.SIGXCPU, "XFSZ": syscall.SIGXFSZ,
	"VTALRM": syscall.SIGVTALRM, "PROF": syscall.SIGPROF, "WINCH": syscall.SIGWINCH,
	"USR1": syscall.SIGUSR1, "USR2": syscall.SIGUSR2,
}

const runningBundleIDsScript = `'use strict';

ObjC.import('AppKit')

function run() {
  var apps = $.NSWorkspace.sharedWorkspace.runningApplications
  var bundleIds = []

  for (var i = 0; i < apps.count; i++) {
    var bundleId = apps.objectAtIndex(i).bundleIdentifier
    if (!bundleId.isNil()) {
      bundleIds.push(ObjC.unwrap(bundleId))
    }
  }

  return bundleIds.join("\n")
}`

func flattenStrings(raw json.RawMessage) ([]string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	var out []string
	var walk func(any) error
	walk = func(v any) error {
		switch t := v.(type) {
		case string:
			out = append(out, t)
		case []any:
			for _, e := range t {
				if err := walk(e); err != nil {
					return err
				}
			}
		default:
			return fmt.Errorf("unexpected %v", v)
		}
		return nil
	}
	return out, walk(v)
}

type scriptSpec struct {
	executable    string
	hasExecutable bool
	args          []string
	input         string
	mustSucceed   bool
}

func scriptWantsSudo(raw json.RawMessage) bool {
	var o struct {
		Sudo bool `json:"sudo"`
	}
	return json.Unmarshal(raw, &o) == nil && o.Sudo
}

var scriptKeys = []string{"args", "input", "executable", "must_succeed", "sudo", "print_stdout", "print_stderr"}

func (in *Installer) readScript(raw json.RawMessage, description string) (scriptSpec, error) {
	spec := scriptSpec{mustSucceed: true}
	var exe string
	if json.Unmarshal(raw, &exe) == nil {
		spec.executable, spec.hasExecutable = exe, true
		return spec, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return spec, brewError("Unsupported arguments type %s", raw)
	}
	var unknown []string
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if !slices.Contains(scriptKeys, k) {
			unknown = append(unknown, ":"+k)
		}
	}
	inspected := "[" + strings.Join(unknown, ", ") + "]"
	if len(unknown) > 0 {
		in.warnf("Unknown arguments to %s -- %s (ignored). Running `brew update; brew cleanup` will likely fix it.", description, inspected)
	}
	if _, ok := obj["print_stdout"]; ok {
		in.errorf("Some arguments to %s will be ignored -- :%s (overridden).", description, inspected)
	}
	for k, v := range obj {
		var err error
		switch k {
		case "executable":
			if err = json.Unmarshal(v, &spec.executable); err == nil {
				spec.hasExecutable = true
			}
		case "args":
			spec.args, err = flattenStrings(v)
		case "input":
			var parts []string
			if parts, err = flattenStrings(v); err == nil {
				spec.input = strings.Join(parts, "")
			}
		case "must_succeed":
			err = json.Unmarshal(v, &spec.mustSucceed)
		}
		if err != nil {
			return spec, fmt.Errorf("%s: %s: %w", description, k, err)
		}
	}
	return spec, nil
}

func (in *Installer) uninstallScript(ctx context.Context, token, staged, directive string, raw json.RawMessage, force bool) error {
	spec, err := in.readScript(raw, "uninstall :"+directive)
	if err != nil {
		return err
	}
	in.logf("==> Running uninstall script %s", spec.executable)
	if !spec.hasExecutable {
		return brewError("Cask '%s' definition is invalid: uninstall :%s without :executable.", token, directive)
	}
	path, err := in.stagedExecutable(staged, spec.executable)
	if err != nil {
		return err
	}
	if _, found := lookPath(path); !found {
		message := fmt.Sprintf("uninstall script %s does not exist", spec.executable)
		if !force {
			return brewError("%s.", message)
		}
		in.warnf("%s; skipping.", message)
		return nil
	}
	c := brewCommand(false, nil, path, spec.args...)
	c.Stdin, c.Tee = []byte(spec.input), in.Log
	if _, err := in.run(ctx, c); err != nil && spec.mustSucceed {
		return err
	}
	in.sleep(time.Second)
	return nil
}

func lookPath(path string) (string, bool) {
	if filepath.IsAbs(path) {
		_, err := os.Stat(path)
		return path, err == nil
	}
	found, err := exec.LookPath(path)
	return found, err == nil
}

func (in *Installer) stagedExecutable(staged, executable string) (string, error) {
	path := executable
	if path == "~" || strings.HasPrefix(path, "~/") {
		path = in.Where.Home + path[1:]
	}
	abs := path
	if !filepath.IsAbs(path) {
		abs = filepath.Join(staged, path)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return path, nil
	}
	if fi.Mode().Perm()&0o111 == 0 {
		if err := in.chmodX(abs, fi.Mode().Perm()); err != nil {
			return "", err
		}
	}
	return abs, nil
}

func (in *Installer) chmodX(path string, perm fs.FileMode) error {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if in.inside(resolved(in.Where.Prefix), real) {
		return in.makeExecutable(real)
	}
	return os.Chmod(real, perm|0o111) //fsx:allow a script outside the prefix
}

func (in *Installer) uninstallKext(ctx context.Context, kexts []string) error {
	asRoot := func(exe string, args ...string) ([]byte, error) {
		return in.run(ctx, brewCommand(true, nil, exe, args...))
	}
	for _, kext := range kexts {
		in.logf("==> Unloading kernel extension %s", kext)
		loaded, err := asRoot("/usr/sbin/kextstat", "-l", "-b", kext)
		if err != nil {
			return err
		}
		if len(loaded) > 1 {
			if _, err := asRoot("/sbin/kextunload", "-b", kext); err != nil {
				return err
			}
			in.sleep(time.Second)
		}
		found, err := asRoot("/usr/sbin/kextfind", "-b", kext)
		if err != nil {
			return err
		}
		for _, p := range lines(strings.TrimSuffix(string(found), "\n")) {
			in.logf("==> Removing kernel extension %s", p)
			if _, err := asRoot("/bin/rm", "-rf", p); err != nil {
				return err
			}
		}
	}
	return nil
}

// lines keeps each line's newline, which brew passes on to rm for all but the last kext.
func lines(s string) []string {
	out := strings.SplitAfter(s, "\n")
	if len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func (in *Installer) uninstallSignal(ctx context.Context, token string, values []string) error {
	type pair struct{ signal, id string }
	var pairs []pair
	var running []string
	haveRunning := false
	for chunk := range slices.Chunk(values, 2) {
		if len(chunk) != 2 {
			return brewError("Cask '%s' definition is invalid: Each uninstall :signal must consist of 2 elements.", token)
		}
		ids := []string{chunk[1]}
		if strings.Contains(chunk[1], "*") {
			if !haveRunning {
				var ok bool
				if running, ok = in.runningBundleIDs(ctx, chunk[1]); !ok {
					continue
				}
				haveRunning = true
			}
			re := regexp.MustCompile(`(?i)^` + strings.ReplaceAll(regexp.QuoteMeta(chunk[1]), `\*`, ".*") + `$`)
			ids = slices.DeleteFunc(slices.Clone(running), func(id string) bool { return !re.MatchString(id) })
		}
		for _, id := range ids {
			pairs = append(pairs, pair{chunk[0], id})
		}
	}
	for _, p := range pairs {
		pids, err := in.runningProcesses(ctx, p.id)
		if err != nil {
			return err
		}
		if len(pids) == 0 {
			continue
		}
		if host := os.Getenv("__CFBundleIdentifier"); strings.EqualFold(host, p.id) {
			in.warnf("Skipping signalling application '%s' as `brw` is running inside it.", p.id)
			continue
		}
		pids = slices.DeleteFunc(pids, func(pid int) bool { return !in.ownedByMe(ctx, pid) })
		if len(pids) == 0 {
			continue
		}
		in.logf("==> Signalling '%s' to application ID '%s'", p.signal, p.id)
		if err := in.killAll(p.signal, p.id, pids); err != nil {
			return err
		}
		in.sleep(signalPause)
	}
	return nil
}

func (in *Installer) killAll(name, id string, pids []int) error {
	sig, ok := signals[strings.TrimPrefix(name, "SIG")]
	if !ok {
		return fmt.Errorf("unsupported signal 'SIG%s'", strings.TrimPrefix(name, "SIG"))
	}
	kill := in.Kill
	if kill == nil {
		kill = func(pid int, sig syscall.Signal) error { return syscall.Kill(pid, sig) }
	}
	for _, pid := range pids {
		if err := kill(pid, sig); errors.Is(err, syscall.EPERM) {
			strs := make([]string, len(pids))
			for i, p := range pids {
				strs[i] = strconv.Itoa(p)
			}
			in.warnf("Failed to kill %s PIDs %s with signal %s: %s", id, strings.Join(strs, ", "), name, eperm)
			return nil
		} else if err != nil {
			return err
		}
	}
	return nil
}

func (in *Installer) runningProcesses(ctx context.Context, id string) ([]int, error) {
	out, err := in.exec(ctx, "/bin/launchctl", "list")
	if err != nil {
		return nil, err
	}
	re := regexp.MustCompile(`^(?:application\.)?` + regexp.QuoteMeta(id) + `(?:\.\d+){0,2}$`)
	var pids []int
	for i, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, "\t")
		if i == 0 || len(f) < 3 {
			continue
		}
		if pid, _ := strconv.Atoi(f[0]); pid != 0 && re.MatchString(f[2]) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func (in *Installer) ownedByMe(ctx context.Context, pid int) bool {
	out, err := in.exec(ctx, "/bin/ps", "-o", "ruid=", "-p", strconv.Itoa(pid))
	if err != nil {
		return false
	}
	uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	return err == nil && uid == os.Getuid()
}

func (in *Installer) runningBundleIDs(ctx context.Context, pattern string) ([]string, bool) {
	if !in.gui(ctx) {
		in.warnf("Not logged into a GUI; skipping applications matching '%s'.", pattern)
		return nil, false
	}
	out, _ := in.exec(ctx, "osascript", "-l", "JavaScript", "-e", runningBundleIDsScript)
	var ids []string
	for id := range strings.SplitSeq(string(out), "\n") {
		if id = strings.TrimSpace(id); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, true
}

func (in *Installer) gui(ctx context.Context) bool {
	u, err := user.Current()
	if err != nil {
		return false
	}
	out, err := in.exec(ctx, "who")
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(out), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[0] == u.Username && f[1] == "console" {
			return true
		}
	}
	return false
}
