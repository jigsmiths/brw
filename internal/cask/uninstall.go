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
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/jigsmiths/brw/internal/api"
)

// Exec runs a command and returns its standard output. Installer.Exec
// replaces it in tests.
type Exec func(ctx context.Context, name string, args ...string) ([]byte, error)

func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out, nil
}

func (in *Installer) exec(ctx context.Context, name string, args ...string) ([]byte, error) {
	if in.Exec != nil {
		return in.Exec(ctx, name, args...)
	}
	return execCommand(ctx, name, args...)
}

// NotInstalled is brew's error for a cask that is not installed.
func NotInstalled(token string) error {
	return brewError("Cask '%s' is not installed.", token)
}

// IsPinned is brew's error for a pinned cask.
func IsPinned(token string) error {
	return brewError("%s is pinned. You must unpin it to uninstall.", token)
}

// uninstallStep is one artifact from the receipt, in the order brew undoes
// them: the uninstall stanza, then apps, then binaries.
type uninstallStep struct {
	kind       string
	directives map[string][]string // for "uninstall"
	move       move                // for "app" and "binary"
}

var stepOrder = map[string]int{"uninstall": 0, "app": 1, "binary": 2}

// Uninstall removes an installed version of a cask as brew does, from the
// artifacts its receipt recorded. It refuses, before changing anything, a
// cask that needs something brw cannot undo yet. force also removes every
// other version and does not stop at an app in the way.
func (in *Installer) Uninstall(ctx context.Context, token, version string, force bool) error {
	steps, err := in.readUninstall(token, version)
	if err != nil {
		return err
	}
	in.logf("==> Uninstalling Cask %s", token)
	for _, s := range steps {
		switch s.kind {
		case "uninstall":
			if err := in.runDirectives(ctx, s.directives); err != nil {
				return fmt.Errorf("%s: %w", token, err)
			}
		case "app":
			if err := in.moveBack(token, version, s.move, force); err != nil {
				return fmt.Errorf("%s: %w", token, err)
			}
		case "binary":
			if err := in.unlinkBinary(s.move); err != nil {
				return fmt.Errorf("%s: %w", token, err)
			}
		}
	}
	meta := filepath.Join("Caskroom", token, ".metadata")
	for _, name := range []string{"INSTALL_RECEIPT.json", "LATEST_DOWNLOAD_SHA256", "config.json"} {
		if err := removeIfThere(in.Root, filepath.Join(meta, name)); err != nil {
			return err
		}
	}
	in.logf("==> Purging files for version %s of Cask %s", version, token)
	if err := in.purge(api.Cask{Token: token, Version: version}); err != nil {
		return err
	}
	if force {
		return removeIfThere(in.Root, filepath.Join("Caskroom", token))
	}
	return nil
}

// readUninstall reads the receipt's uninstall_artifacts, filled in when the
// cask was installed, and checks that brw can undo every one.
func (in *Installer) readUninstall(token, version string) ([]uninstallStep, error) {
	unsupported := func(what string) error {
		return fmt.Errorf("%s: %s: %w", token, what, ErrUnsupported)
	}
	b, err := in.Root.ReadFile(filepath.Join("Caskroom", token, ".metadata", "INSTALL_RECEIPT.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, unsupported("no install receipt")
	}
	if err != nil {
		return nil, err
	}
	var receipt struct {
		UninstallFlightBlocks bool                         `json:"uninstall_flight_blocks"`
		UninstallArtifacts    []map[string]json.RawMessage `json:"uninstall_artifacts"`
	}
	if err := json.Unmarshal(b, &receipt); err != nil {
		return nil, fmt.Errorf("%s: install receipt: %w", token, err)
	}
	if receipt.UninstallFlightBlocks {
		return nil, unsupported("uninstall flight blocks")
	}
	if len(receipt.UninstallArtifacts) == 0 {
		return nil, unsupported("no uninstall artifacts in the install receipt")
	}
	var steps []uninstallStep
	for _, a := range receipt.UninstallArtifacts {
		for kind, args := range a {
			s := uninstallStep{kind: kind}
			switch kind {
			case "zap":
				continue
			case "uninstall":
				if bad := unknownDirectives(args); bad != "" {
					return nil, unsupported("uninstall " + bad)
				}
				if s.directives, err = readDirectives(args); err != nil {
					return nil, fmt.Errorf("%s: uninstall %s: %w", token, args, err)
				}
			case "app", "binary":
				if s.move, err = readMove(args, kind, in.Where); err != nil {
					return nil, fmt.Errorf("%s: %s %s: %w", token, kind, args, err)
				}
				if kind == "app" {
					_, err = in.appRel(s.move.target)
				} else {
					_, err = in.prefixRel(s.move.target)
				}
				if err != nil {
					return nil, fmt.Errorf("%s: %w", token, err)
				}
				if kind == "app" {
					if filepath.IsAbs(s.move.source) {
						return nil, unsupported("app with an absolute source")
					}
					s.move.source = filepath.Join("Caskroom", token, version, s.move.source)
				}
			default:
				return nil, unsupported(kind + " artifacts")
			}
			steps = append(steps, s)
		}
	}
	slices.SortStableFunc(steps, func(a, b uninstallStep) int { return stepOrder[a.kind] - stepOrder[b.kind] })
	return steps, nil
}

// readDirectives reads an uninstall stanza: a list of objects whose values
// are a string or a list of strings.
func readDirectives(args json.RawMessage) (map[string][]string, error) {
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(args, &list); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, d := range list {
		for k, v := range d {
			var one string
			if json.Unmarshal(v, &one) == nil {
				out[k] = append(out[k], one)
				continue
			}
			var many []string
			if err := json.Unmarshal(v, &many); err != nil {
				return nil, fmt.Errorf("%s: %w", k, err)
			}
			out[k] = append(out[k], many...)
		}
	}
	return out, nil
}

// runDirectives carries out the uninstall stanza in brew's order. rmdir is
// brew's post-uninstall phase, which follows straight after.
func (in *Installer) runDirectives(ctx context.Context, d map[string][]string) error {
	if s := d["launchctl"]; len(s) > 0 {
		in.launchctl(ctx, s)
	}
	if s := d["quit"]; len(s) > 0 {
		in.quit(ctx, s)
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
		in.rmdirs(ctx, s)
	}
	return nil
}

// launchctl removes services as brew does: from the user's domain, then from
// the system's with sudo, with their plists. brw looks a system service up
// without sudo, so it asks for a password only when there is one to remove.
func (in *Installer) launchctl(ctx context.Context, services []string) {
	var all []string
	for _, s := range services {
		if !strings.Contains(s, "*") {
			all = append(all, s)
			continue
		}
		all = append(all, in.runningServices(ctx, s)...)
	}
	uid := fmt.Sprint(os.Getuid())
	for _, s := range all {
		in.logf("==> Removing launchctl service %s", s)
		for _, sudo := range []bool{false, true} {
			found := false
			if sudo {
				_, err := in.exec(ctx, "/bin/launchctl", "print", "system/"+s)
				found = err == nil
			} else {
				for _, domain := range []string{"user/" + uid, "gui/" + uid} {
					if _, err := in.exec(ctx, "/bin/launchctl", "print", domain+"/"+s); err == nil {
						found = true
						break
					}
				}
			}
			if found {
				if _, err := in.privileged(ctx, sudo, "/bin/launchctl", "remove", s); err == nil {
					time.Sleep(in.wait(time.Second))
				}
			}
			for _, dir := range []string{"/Library/LaunchAgents", "/Library/LaunchDaemons"} {
				if !sudo {
					dir = filepath.Join(in.Where.Home, dir)
				}
				plist := filepath.Join(dir, s+".plist")
				if _, err := os.Lstat(plist); err == nil {
					_, _ = in.privileged(ctx, sudo, "/bin/rm", "-f", "--", plist)
				}
			}
		}
	}
}

func (in *Installer) privileged(ctx context.Context, sudo bool, name string, args ...string) ([]byte, error) {
	if sudo {
		return in.exec(ctx, "/usr/bin/sudo", append([]string{"-E", "--", name}, args...)...)
	}
	return in.exec(ctx, name, args...)
}

// wait shortens brew's pauses in tests.
func (in *Installer) wait(d time.Duration) time.Duration {
	if in.Exec != nil {
		return 0
	}
	return d
}

// runningServices are the running launchd services that match a pattern
// with * in it.
func (in *Installer) runningServices(ctx context.Context, pattern string) []string {
	out, err := in.exec(ctx, "/bin/launchctl", "list")
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

// quit asks running apps to quit, giving each ten seconds, as brew does. It
// leaves alone the app brw runs in, such as the terminal.
func (in *Installer) quit(ctx context.Context, ids []string) {
	for _, id := range ids {
		if strings.Contains(id, "*") {
			in.warnf("brw cannot quit applications matching '%s' yet; skipping.", id)
			continue
		}
		if !in.running(ctx, id) {
			continue
		}
		if host := os.Getenv("__CFBundleIdentifier"); strings.EqualFold(host, id) {
			in.warnf("Skipping quitting application '%s' as `brw` is running inside it.", id)
			continue
		}
		in.logf("==> Quitting application '%s'...", id)
		deadline := time.Now().Add(10 * time.Second)
		quit := false
		for time.Now().Before(deadline) {
			if _, err := in.exec(ctx, "osascript", "-l", "JavaScript", "-e", quitScript, id); err == nil && !in.running(ctx, id) {
				in.logf("Application '%s' quit successfully.", id)
				quit = true
				break
			}
			time.Sleep(in.wait(100 * time.Millisecond))
		}
		if !quit {
			in.warnf("Application '%s' did not quit. %s", id, automationAccess)
		}
	}
}

func (in *Installer) running(ctx context.Context, id string) bool {
	_, err := in.exec(ctx, "osascript", "-l", "JavaScript", "-e", runningScript, id)
	return err == nil
}

// resolvePaths expands ~ and globs a directive's paths, skipping, with
// brew's warnings, relative paths and those whose folder brw cannot write.
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
			if !writable(filepath.Dir(m)) {
				in.warnf("Skipping %s for undeletable path '%s'.", action, m)
				continue
			}
			resolved = append(resolved, m)
		}
		each(p, resolved)
	}
}

func writable(dir string) bool {
	return syscall.Access(dir, 0x2) == nil // W_OK
}

// deletePaths is brew's delete directive. brew always uses sudo; brw tries
// without first, so most casks need no password.
func (in *Installer) deletePaths(ctx context.Context, paths []string) error {
	in.logf("==> Removing files:")
	var err error
	in.resolvePaths("delete", paths, func(p string, resolved []string) {
		in.logf("%s", p)
		if len(resolved) == 0 {
			return
		}
		args := append([]string{"-r", "-f", "--"}, resolved...)
		if _, e := in.exec(ctx, "/bin/rm", args...); e != nil {
			if _, e := in.privileged(ctx, true, "/bin/rm", args...); e != nil {
				err = errors.Join(err, e)
			}
		}
	})
	return err
}

// trash moves paths to the user's Trash with macOS's trash command.
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
	var failed []string
	for _, p := range all {
		if _, err := in.exec(ctx, "/usr/bin/trash", p); err != nil {
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

// rmdirs removes directories that hold nothing but empty directories and
// .DS_Store files.
func (in *Installer) rmdirs(ctx context.Context, dirs []string) {
	in.logf("==> Removing directories if empty:")
	in.resolvePaths("rmdir", dirs, func(_ string, resolved []string) {
		for _, r := range resolved {
			if fi, err := os.Stat(r); err != nil || !fi.IsDir() {
				return
			}
		}
		in.rmdirAll(ctx, resolved)
	})
}

func (in *Installer) rmdirAll(ctx context.Context, dirs []string) bool {
	for _, d := range dirs {
		in.logf("%s", tildeHome(d, in.Where.Home))
		if fi, err := os.Lstat(d); err != nil || fi.Mode()&fs.ModeSymlink != 0 {
			return false
		}
		entries, err := os.ReadDir(d)
		if err != nil {
			return false
		}
		var children []string
		for _, e := range entries {
			p := filepath.Join(d, e.Name())
			if e.Name() == ".DS_Store" {
				continue
			}
			if fi, err := os.Stat(p); err != nil || !fi.IsDir() {
				return false
			}
			children = append(children, p)
		}
		if _, err := os.Lstat(filepath.Join(d, ".DS_Store")); err == nil {
			_, _ = in.exec(ctx, "/bin/rm", "-f", "--", filepath.Join(d, ".DS_Store"))
		}
		if !in.rmdirAll(ctx, children) {
			return false
		}
		if _, err := in.exec(ctx, "/bin/rmdir", "--", d); err != nil {
			return false
		}
	}
	return true
}

// tildeHome is Ruby's sub: the first occurrence of home becomes ~.
func tildeHome(path, home string) string {
	if home == "" {
		return path
	}
	return strings.Replace(path, home, "~", 1)
}

// moveBack is brew's move_back: the app goes back to the Caskroom, which the
// purge then removes. brew copies and deletes; brw moves, to the same end.
func (in *Installer) moveBack(token, version string, m move, force bool) error {
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
		return nil // brew skips an app that is gone
	}
	in.logf("==> Backing up App '%s' to '%s'", filepath.Base(m.target), abs)
	if err := in.Root.MkdirAll(filepath.Dir(staged), 0o755); err != nil {
		return err
	}
	in.logf("==> Removing App '%s'", m.target)
	return in.Apps.MoveTo(in.Root, rel, staged)
}

// unlinkBinary removes the binary's link, leaving one a formula linked.
func (in *Installer) unlinkBinary(m move) error {
	fi, err := os.Lstat(m.target)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	cellar := filepath.Join(in.Where.Prefix, "Cellar")
	if real, err := filepath.EvalSymlinks(cellar); err == nil {
		cellar = real
	}
	if real, err := filepath.EvalSymlinks(m.target); err == nil && in.inside(cellar, real) {
		return nil
	}
	rel, err := in.prefixRel(m.target)
	if err != nil {
		return err
	}
	in.logf("==> Unlinking Binary '%s'", m.target)
	return in.Root.Remove(rel)
}
