package cask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jigsmiths/brw/internal/sudo"
)

const (
	signalPause = 3 * time.Second
	eperm       = "Operation not permitted"
)

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
	installerStep
	hasExecutable bool
}

func (in *Installer) readScript(raw json.RawMessage, description string) (scriptSpec, error) {
	spec := scriptSpec{mustSucceed: true, stderr: outputOn}
	var exe string
	if json.Unmarshal(raw, &exe) == nil {
		spec.path, spec.hasExecutable = exe, true
		return spec, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return spec, brewError("Unsupported arguments type %s", raw)
	}
	keys, err := keyOrder(raw)
	if err != nil {
		return spec, brewError("Unsupported arguments type %s", raw)
	}
	var unknown []string
	for _, k := range keys {
		if !slices.Contains(scriptKeys, k) {
			unknown = append(unknown, k)
		}
	}
	inspected := rubySymbols(unknown)
	if len(unknown) > 0 {
		in.warnf("Unknown arguments to %s -- %s (ignored). Running `brw update; brw cleanup` will likely fix it.", description, inspected)
	}
	if _, ok := obj["print_stdout"]; ok {
		in.errorf("Some arguments to %s will be ignored -- :%s (overridden).", description, inspected)
	}
	for _, k := range keys {
		if !slices.Contains(scriptKeys, k) {
			continue
		}
		if err := spec.set(k, obj[k]); err != nil {
			return spec, fmt.Errorf("%s: %s: %w", description, k, err)
		}
		spec.hasExecutable = spec.hasExecutable || k == "executable"
	}
	return spec, nil
}

func (in *Installer) uninstallScript(ctx context.Context, token, staged, directive string, raw json.RawMessage, force bool) error {
	spec, err := in.readScript(raw, "uninstall :"+directive)
	if err != nil {
		return err
	}
	in.logf("==> Running uninstall script %s", spec.path)
	if !spec.hasExecutable {
		return brewError("Cask '%s' definition is invalid: uninstall :%s without :executable.", token, directive)
	}
	path, err := in.stagedExecutable(staged, spec.path)
	if err != nil {
		return err
	}
	if _, found := lookPath(path); !found {
		message := fmt.Sprintf("uninstall script %s does not exist", spec.path)
		if !force {
			return brewError("%s.", message)
		}
		in.warnf("%s; skipping.", message)
		return nil
	}
	if err := in.runScript(ctx, spec.installerStep, path, nil); err != nil {
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

func (in *Installer) uninstallKext(ctx context.Context, kexts []string) error {
	asRoot := func(exe string, args ...string) (string, error) {
		return in.capture(ctx, in.command(sudo.Root, exe, args...))
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
		for _, p := range lines(strings.TrimSuffix(found, "\n")) {
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

func (in *Installer) uninstallSignal(ctx context.Context, token string, values []string, expand *bundleIDs) error {
	type pair struct{ signal, id string }
	var pairs []pair
	for chunk := range slices.Chunk(values, 2) {
		if len(chunk) != 2 {
			return brewError("Cask '%s' definition is invalid: Each uninstall :signal must consist of 2 elements.", token)
		}
		for _, id := range expand.expand(ctx, chunk[1]) {
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
		if in.hostingBrw(ctx, p.id) {
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
	sig, err := sudo.ParseSignal(name)
	if err != nil {
		return err
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
	out, err := in.system(ctx, "/bin/launchctl", "list")
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
