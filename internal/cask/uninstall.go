package cask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/sudo"
)

type Exec func(ctx context.Context, name string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	sudo.Guard(name)
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func (in *Installer) system(ctx context.Context, name string, args ...string) ([]byte, error) {
	if in.debug() && (in.Verbose || sudo.EnvTrue("HOMEBREW_VERBOSE")) && in.Warn != nil {
		_, _ = fmt.Fprintln(in.Warn, sudo.Redact(sudo.ShellJoin(append([]string{"/usr/bin/env", name}, args...)), sudo.SensitiveValues()))
	}
	return in.exec(ctx, name, args...)
}

func (in *Installer) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if in.Exec != nil {
		return in.Exec(ctx, name, args...)
	}
	return execCommand(ctx, name, args...)
}

func NotInstalled(token string) error {
	return brewError("Cask '%s' is not installed.", token)
}

func IsPinned(token string) error {
	return brewError("%s is pinned. You must unpin it to uninstall.", token)
}

type uninstallStep struct {
	kind          string
	directives    map[string][]string
	scripts       map[string][]json.RawMessage
	move          move
	generator     generator
	steps         []json.RawMessage
	version       string
	stagedVersion string
}

var stepOrder = map[string]int{"uninstall": 0, "app": 1,
	"binary": 2, "manpage": 2, "bash_completion": 2, "zsh_completion": 2, "fish_completion": 2,
	"generate_completions_from_executable": 3, "preflight_steps": 4, "postflight_steps": 4}

// Uninstall refuses, before changing anything, a cask that needs something brw cannot undo yet. force also removes every other version.
func (in *Installer) Uninstall(ctx context.Context, token, version string, force bool) error {
	steps, err := in.readUninstall(token, version)
	if err != nil {
		return err
	}
	in.logf("==> Uninstalling Cask %s", token)
	if err := in.removeArtifacts(ctx, token, steps, force); err != nil {
		return err
	}
	meta := filepath.Join("Caskroom", token, ".metadata")
	for _, name := range []string{"INSTALL_RECEIPT.json", "LATEST_DOWNLOAD_SHA256", "config.json"} {
		if err := removeIfThere(in.Root, filepath.Join(meta, name)); err != nil {
			return err
		}
	}
	in.logf("==> Purging files for version %s of Cask %s", version, token)
	if err := in.purge(ctx, api.Cask{Token: token, Version: version}); err != nil {
		return err
	}
	if force {
		return in.removePath(ctx, in.Root, filepath.Join("Caskroom", token))
	}
	return nil
}

func (in *Installer) removeArtifacts(ctx context.Context, token string, steps []uninstallStep, force bool) error {
	var apps []string
	for _, s := range steps {
		if s.kind == "app" {
			apps = append(apps, s.move.target)
		}
	}
	for _, s := range steps {
		switch s.kind {
		case "uninstall":
			if err := in.runDirectives(ctx, token, s, apps, force); err != nil {
				return fmt.Errorf("%s: %w", token, err)
			}
		case "app":
			if err := in.moveBack(ctx, s.move, force); err != nil {
				return fmt.Errorf("%s: %w", token, err)
			}
		case "binary", "manpage", "bash_completion", "zsh_completion", "fish_completion":
			if err := in.unlinkBinary(s.move); err != nil {
				return fmt.Errorf("%s: %w", token, err)
			}
		case "generate_completions_from_executable":
			in.removeCompletions(token, s.generator)
		case "preflight_steps", "postflight_steps":
			if err := poststep.Uninstall(in.stepContext(token, s.version, s.stagedVersion), s.steps); err != nil {
				return fmt.Errorf("%s: %s: %w", token, s.kind, err)
			}
		}
	}
	return nil
}

func (in *Installer) readUninstall(token, version string) ([]uninstallStep, error) {
	artifacts, err := in.readReceipt(token)
	if err != nil {
		return nil, err
	}
	var steps []uninstallStep
	for _, a := range artifacts {
		for kind, args := range a {
			if kind == "zap" || kind == "installer" {
				continue
			}
			s, err := in.readStep(token, version, kind, args)
			if err != nil {
				return nil, err
			}
			steps = append(steps, s)
		}
	}
	slices.SortStableFunc(steps, func(a, b uninstallStep) int { return stepOrder[a.kind] - stepOrder[b.kind] })
	return steps, nil
}

func unsupported(token, what string) error {
	return fmt.Errorf("%s: %s: %w", token, what, ErrUnsupported)
}

func (in *Installer) readReceipt(token string) ([]map[string]json.RawMessage, error) {
	b, err := in.Root.ReadFile(filepath.Join("Caskroom", token, ".metadata", "INSTALL_RECEIPT.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, unsupported(token, "no install receipt")
	}
	if err != nil {
		return nil, err
	}
	var receipt struct {
		UninstallFlightBlocks bool            `json:"uninstall_flight_blocks"`
		UninstallArtifacts    json.RawMessage `json:"uninstall_artifacts"`
	}
	if err := json.Unmarshal(b, &receipt); err != nil {
		return nil, fmt.Errorf("%s: install receipt: %w", token, err)
	}
	if receipt.UninstallFlightBlocks {
		return nil, unsupported(token, "uninstall flight blocks")
	}
	var artifacts []map[string]json.RawMessage
	if err := json.Unmarshal(receipt.UninstallArtifacts, &artifacts); err != nil || artifacts == nil {
		return nil, unsupported(token, "no uninstall artifacts in the install receipt")
	}
	return artifacts, nil
}

func (in *Installer) readStep(token, version, kind string, args json.RawMessage) (uninstallStep, error) {
	s := uninstallStep{kind: kind}
	var err error
	switch kind {
	case "uninstall":
		if bad := unknownDirectives(args); bad != "" {
			return s, unsupported(token, "uninstall "+bad)
		}
		if s.directives, s.scripts, err = readDirectives(args); err != nil {
			return s, fmt.Errorf("%s: uninstall %s: %w", token, args, err)
		}
		if s.stagedVersion, err = in.Root.Abs(filepath.Join("Caskroom", token, version)); err != nil {
			return s, err
		}
	case "app", "binary", "manpage", "bash_completion", "zsh_completion", "fish_completion":
		if s.move, err = readMove(args, kind, in.Where); err != nil {
			return s, fmt.Errorf("%s: %s %s: %w", token, kind, args, err)
		}
		if kind != "app" {
			if _, err := in.prefixRel(s.move.target); err != nil {
				return s, fmt.Errorf("%s: %w", token, err)
			}
			return s, nil
		}
		if _, err := in.appRel(s.move.target); err != nil {
			return s, fmt.Errorf("%s: %w", token, err)
		}
		if filepath.IsAbs(s.move.source) {
			return s, unsupported(token, "app with an absolute source")
		}
		s.move.source = filepath.Join("Caskroom", token, version, s.move.source)
	case "generate_completions_from_executable":
		if s.generator, err = readGenerator(args); err != nil {
			return s, fmt.Errorf("%s: %s %s: %w", token, kind, args, err)
		}
	case "preflight_steps", "postflight_steps":
		if s.steps, err = readSteps(args); err != nil {
			return s, fmt.Errorf("%s: %s %s: %w", token, kind, args, err)
		}
		s.version = version
		if s.stagedVersion, err = in.Root.Abs(filepath.Join("Caskroom", token, version)); err != nil {
			return s, err
		}
	default:
		return s, unsupported(token, kind+" artifacts")
	}
	return s, nil
}

func readDirectives(args json.RawMessage) (map[string][]string, map[string][]json.RawMessage, error) {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(args, &list); err != nil {
		return nil, nil, err
	}
	out := map[string][]string{}
	scripts := map[string][]json.RawMessage{}
	for _, d := range list {
		for k, v := range d {
			if slices.Contains(scriptDirectives, k) {
				scripts[k] = append(scripts[k], v)
				continue
			}
			values, err := flattenStrings(v)
			if err != nil {
				return nil, nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = append(out[k], values...)
		}
	}
	return out, scripts, nil
}

func (in *Installer) runDirectives(ctx context.Context, token string, step uninstallStep, apps []string, force bool) error {
	d := step.directives
	expand := &bundleIDs{in: in}
	runScripts := func(directive string) error {
		for _, raw := range step.scripts[directive] {
			if err := in.uninstallScript(ctx, token, step.stagedVersion, directive, raw, force); err != nil {
				return err
			}
		}
		return nil
	}
	if err := runScripts("early_script"); err != nil {
		return err
	}
	if s := d["launchctl"]; len(s) > 0 {
		if err := in.launchctl(ctx, s); err != nil {
			return err
		}
	}
	if s := d["quit"]; len(s) > 0 {
		in.quit(ctx, s, expand)
	}
	if s := d["signal"]; len(s) > 0 && (!in.upgrading || slices.Contains(d["on_upgrade"], "signal")) {
		if err := in.uninstallSignal(ctx, token, s, expand); err != nil {
			return err
		}
	}
	if s := d["login_item"]; len(s) > 0 {
		in.loginItems(ctx, apps, s)
	}
	if s := d["kext"]; len(s) > 0 {
		if err := in.uninstallKext(ctx, s); err != nil {
			return err
		}
	}
	if err := runScripts("script"); err != nil {
		return err
	}
	if s := d["pkgutil"]; len(s) > 0 {
		if err := in.uninstallPkgutil(ctx, s); err != nil {
			return err
		}
	}
	if s := d["delete"]; len(s) > 0 {
		if err := in.deletePaths(ctx, s); err != nil {
			return err
		}
	}
	if s := d["trash"]; len(s) > 0 {
		in.trash(ctx, s)
	}
	if s := d["rmdir"]; len(s) > 0 {
		if err := in.rmdirs(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (in *Installer) launchctl(ctx context.Context, services []string) error {
	var all []string
	for _, s := range services {
		if !strings.Contains(s, "*") {
			all = append(all, s)
			continue
		}
		all = append(all, in.runningServices(ctx, s)...)
	}
	for _, s := range all {
		in.logf("==> Removing launchctl service %s", s)
		for _, asRoot := range []bool{false, true} {
			if asRoot && !sudo.Available(ctx, in.runner()) {
				continue
			}
			privilege := sudo.User
			if asRoot {
				privilege = sudo.Root
			}
			found, err := in.FindService(ctx, s, privilege)
			if err != nil {
				return err
			}
			if found {
				if err := in.privileged(ctx, privilege, "/bin/launchctl", "remove", s); err == nil {
					in.sleep(time.Second)
				}
			}
			for _, dir := range []string{"/Library/LaunchAgents", "/Library/LaunchDaemons"} {
				if !asRoot {
					dir = filepath.Join(in.Where.Home, dir)
				}
				plist := filepath.Join(dir, s+".plist")
				if _, err := os.Lstat(plist); err == nil {
					_ = in.privileged(ctx, privilege, "/bin/rm", "-f", "--", plist)
				}
			}
			if _, err := os.Stat(s); err == nil {
				_ = in.privileged(ctx, privilege, "/bin/launchctl", "unload", "-w", "--", s)
				_ = in.privileged(ctx, privilege, "/bin/rm", "-f", "--", s)
				in.sleep(time.Second)
			}
		}
	}
	return nil
}

func (in *Installer) privileged(ctx context.Context, privilege sudo.Privilege, name string, args ...string) error {
	if privilege == sudo.User {
		_, err := in.system(ctx, name, args...)
		return err
	}
	_, err := in.capture(ctx, in.command(privilege, name, args...))
	return err
}

func (in *Installer) sleep(d time.Duration) {
	if in.Sleep != nil {
		in.Sleep(d)
		return
	}
	time.Sleep(d)
}

func (in *Installer) runningServices(ctx context.Context, pattern string) []string {
	out, err := in.system(ctx, "/bin/launchctl", "list")
	if err != nil {
		return nil
	}
	re := regexp.MustCompile(`^` + strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, `.*`) + `$`)
	var found []string
	for i, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if i == 0 || len(f) < 3 || f[0] == "-" || f[0] == "0" {
			continue
		}
		if re.MatchString(f[2]) {
			found = append(found, f[2])
		}
	}
	return found
}

const runningScript = `'use strict';
ObjC.import('stdlib')
function run(argv) {
  try {
    var app = Application(argv[0])
    if (app.running()) {
      $.exit(0)
    }
  } catch (err) { }
  $.exit(1)
}`

const quitScript = `'use strict';
ObjC.import('stdlib')
function run(argv) {
  var app = Application(argv[0])
  try {
    app.quit()
  } catch (err) {
    if (app.running()) {
      $.exit(1)
    }
  }
  $.exit(0)
}`

const automationAccess = `Enable Automation access for "Terminal → System Events" in:
  System Settings → Privacy & Security → Automation
if you haven't already.`

func (in *Installer) quit(ctx context.Context, patterns []string, expand *bundleIDs) {
	var ids []string
	for _, p := range patterns {
		ids = append(ids, expand.expand(ctx, p)...)
	}
	for _, id := range ids {
		if !in.running(ctx, id) {
			continue
		}
		if !in.gui(ctx) {
			in.warnf("Not logged into a GUI; skipping quitting application ID '%s'.", id)
			continue
		}
		if in.hostingBrw(ctx, id) {
			in.warnf("Skipping quitting application '%s' as `brw` is running inside it.", id)
			continue
		}
		in.logf("==> Quitting application '%s'...", id)
		deadline := time.Now().Add(10 * time.Second)
		quit := false
		for time.Now().Before(deadline) {
			if _, err := in.system(ctx, "osascript", "-l", "JavaScript", "-e", quitScript, id); err == nil && !in.running(ctx, id) {
				in.logf("Application '%s' quit successfully.", id)
				quit = true
				if in.upgrading {
					in.reopen = append(in.reopen, id)
				}
				break
			}
		}
		if !quit {
			in.warnf("Application '%s' did not quit. %s", id, automationAccess)
		}
	}
}

func (in *Installer) loginItems(ctx context.Context, apps, names []string) {
	if in.upgrading {
		return
	}
	remove := func(property, id string) {
		in.logf("==> Removing login item %s", id)
		quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(id)
		script := `tell application "System Events" to delete every login item whose ` + property + ` is "` + quoted + `"`
		if _, err := in.system(ctx, "osascript", "-e", script); err != nil {
			in.warnf("Removal of login item %s failed. %s", id, automationAccess)
		}
		in.sleep(time.Second)
	}
	for _, a := range apps {
		remove("path", a)
	}
	for _, n := range names {
		remove("name", n)
	}
}

func (in *Installer) running(ctx context.Context, id string) bool {
	_, err := in.system(ctx, "osascript", "-l", "JavaScript", "-e", runningScript, id)
	return err == nil
}

func (in *Installer) resolvePaths(action string, paths []string, each func(path string, resolved []string)) {
	for _, p := range paths {
		r := p
		if r == "~" || strings.HasPrefix(r, "~/") {
			r = in.Where.Home + r[1:]
		}
		if !filepath.IsAbs(r) {
			in.warnf("Skipping %s for relative path '%s'.", action, p)
			continue
		}
		if slices.ContainsFunc(strings.Split(r, "/"), func(s string) bool { return s == "." || s == ".." }) {
			in.warnf("Skipping %s for path with relative segments '%s'.", action, p)
			continue
		}
		matches, _ := filepath.Glob(r)
		var resolved []string
		for _, m := range matches {
			if undeletable(m, in.Where.Home) {
				in.warnf("Skipping %s for undeletable path '%s'.", action, m)
				continue
			}
			resolved = append(resolved, m)
		}
		each(p, resolved)
	}
}

const trashScript = `'use strict';
ObjC.import('Foundation')
function run(argv) {
  var fm = $.NSFileManager.defaultManager
  return argv.map(function (p) {
    return fm.trashItemAtURLResultingItemURLError($.NSURL.fileURLWithPath(p), null, null) ? '1' : '0'
  }).join('\n')
}`

const (
	accessWrite = 0x2
	accessRead  = 0x4
)

func writable(dir string) bool {
	return syscall.Access(dir, accessWrite) == nil
}

func (in *Installer) deletePaths(ctx context.Context, paths []string) error {
	in.logf("==> Removing files:")
	var err error
	in.resolvePaths("delete", paths, func(p string, resolved []string) {
		if err != nil {
			return
		}
		in.logf("%s", p)
		if len(resolved) == 0 {
			return
		}
		rm := in.command(sudo.Sudo, xargsPath, "-0", "--", "/bin/rm", "-r", "-f", "--")
		rm.Input = strings.Join(resolved, "\x00")
		_, err = in.capture(ctx, rm)
	})
	return err
}

func (in *Installer) trash(ctx context.Context, paths []string) {
	var shown, all []string
	in.resolvePaths("trash", paths, func(p string, resolved []string) {
		shown = append(shown, p)
		all = append(all, resolved...)
	})
	in.logf("==> Trashing files:")
	for _, p := range shown {
		in.logf("%s", p)
	}
	trashed := in.trashItems(ctx, all)
	var failed []string
	for i, p := range all {
		if trashed[i] {
			continue
		}
		retry := func(p string) error {
			if !in.trashItems(ctx, []string{p})[0] {
				return errors.New("not trashed")
			}
			return nil
		}
		if in.gainPermissions(ctx, p, []string{"-R"}, retry) != nil {
			failed = append(failed, p)
		}
	}
	if len(failed) > 0 {
		in.warnf("The following files could not be trashed, please do so manually:")
		if in.Warn != nil {
			_, _ = fmt.Fprintln(in.Warn, strings.Join(failed, "\n"))
		}
	}
}

func (in *Installer) trashItems(ctx context.Context, paths []string) []bool {
	trashed := make([]bool, len(paths))
	if len(paths) == 0 {
		return trashed
	}
	out, err := in.exec(ctx, "osascript", append([]string{"-l", "JavaScript", "-e", trashScript}, paths...)...)
	results := strings.Fields(string(out))
	if err != nil || len(results) != len(paths) {
		return trashed
	}
	for i, r := range results {
		trashed[i] = r == "1"
	}
	return trashed
}

func (in *Installer) rmdirs(ctx context.Context, dirs []string) error {
	in.logf("==> Removing directories if empty:")
	var err error
	in.resolvePaths("rmdir", dirs, func(_ string, resolved []string) {
		for _, r := range resolved {
			if fi, e := os.Stat(r); e != nil || !fi.IsDir() {
				return
			}
		}
		if err == nil {
			_, err = in.rmdirAll(ctx, resolved)
		}
	})
	return err
}

const dsStore = ".DS_Store"

func (in *Installer) rmdirAll(ctx context.Context, dirs []string) (bool, error) {
	for _, d := range dirs {
		in.logf("%s", tildeHome(d, in.Where.Home))
		if fi, err := os.Lstat(d); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
			return false, nil
		}
		children, ok, err := in.rmdirChildren(ctx, d)
		if err != nil || !ok {
			return false, err
		}
		if ds := filepath.Join(d, dsStore); slices.Contains(children, ds) {
			if err := in.gainPermissionsRemove(ctx, ds); err != nil {
				return false, err
			}
			children = slices.DeleteFunc(children, func(c string) bool { return c == ds })
		}
		if ok, err := in.rmdirAll(ctx, children); err != nil || !ok {
			return false, err
		}
		if err := in.gainPermissionsRmdir(ctx, d); err != nil {
			if errors.Is(err, syscall.ENOTEMPTY) || sudo.IsFailure(err) {
				return false, nil
			}
			return false, err
		}
	}
	return true, nil
}

func (in *Installer) rmdirChildren(ctx context.Context, dir string) (children []string, ok bool, err error) {
	if syscall.Access(dir, accessRead) == nil {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, false, err
		}
		for _, e := range entries {
			p := filepath.Join(dir, e.Name())
			if fi, err := os.Stat(p); e.Name() != dsStore && (err != nil || !fi.IsDir()) {
				return nil, false, nil
			}
			children = append(children, p)
		}
		return children, true, nil
	}
	ls := in.command(sudo.Sudo, "/bin/ls", "-A", "-F", "--", dir)
	ls.Stderr = nil
	out, err := in.capture(ctx, ls)
	if err != nil {
		return nil, false, err
	}
	for line := range strings.SplitSeq(strings.TrimSuffix(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		name, isDir := strings.CutSuffix(line, "/")
		if !isDir && line != dsStore {
			return nil, false, nil
		}
		children = append(children, filepath.Join(dir, name))
	}
	return children, true, nil
}

func tildeHome(path, home string) string {
	if home == "" {
		return path
	}
	return strings.Replace(path, home, "~", 1)
}

// moveBack moves the app back to the Caskroom where brew copies and deletes, to the same end.
func (in *Installer) moveBack(ctx context.Context, m move, force bool) error {
	rel, err := in.appRel(m.target)
	if err != nil {
		return err
	}
	staged := m.source
	if fi, err := in.Root.Lstat(staged); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if dest, err := in.Root.Readlink(staged); err == nil && dest == m.target {
			if err := in.Root.Remove(staged); err != nil {
				return err
			}
		}
	}
	abs, err := in.Root.Abs(staged)
	if err != nil {
		return err
	}
	if _, err := in.Root.Lstat(staged); err == nil {
		if !force {
			return brewError("It seems there is already an App at '%s'.", abs)
		}
		in.warnf("It seems there is already an App at '%s'; overwriting.", abs)
		in.logf("==> Removing App '%s'", abs)
		if err := in.Root.RemoveAll(staged); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(m.target); err != nil {
		return nil
	}
	in.logf("==> Backing up App '%s' to '%s'", filepath.Base(m.target), abs)
	if err := in.Root.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		return err
	}
	if writable(filepath.Dir(m.target)) {
		in.logf("==> Removing App '%s'", m.target)
		return in.Apps.MoveTo(in.Root, rel, staged)
	}
	if err := in.sudoIfNeeded(ctx, "/bin/cp", in.backupArgs(m.target, abs)...); err != nil {
		return err
	}
	in.logf("==> Removing App '%s'", m.target)
	return in.removePath(ctx, in.Apps, rel)
}

func (in *Installer) backupArgs(target, dest string) []string {
	args := []string{"-pR", target, dest}
	if runtime.GOOS != "darwin" {
		return args
	}
	if major, err := versionMajor(in.Host.MacVersion); err != nil || major < 14 {
		return args
	}
	a, errA := os.Stat(target)
	b, errB := os.Stat(filepath.Dir(dest))
	if errA != nil || errB != nil {
		return args
	}
	sa, okA := a.Sys().(*syscall.Stat_t)
	sb, okB := b.Sys().(*syscall.Stat_t)
	if !okA || !okB || sa.Dev != sb.Dev {
		return args
	}
	return append([]string{"-c"}, args...)
}

func (in *Installer) unlinkBinary(m move) error {
	fi, err := os.Lstat(m.target)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	if _, ok := in.formulaLink(m.target); ok {
		return nil
	}
	rel, err := in.prefixRel(m.target)
	if err != nil {
		return err
	}
	in.logf("==> Unlinking %s '%s'", linkNames[m.kind], m.target)
	return in.Root.Remove(rel)
}
