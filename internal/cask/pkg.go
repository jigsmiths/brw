package cask

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/sudo"
)

const (
	installerPath = "/usr/sbin/installer"
	pkgutilPath   = "/usr/sbin/pkgutil"
	xargsPath     = "/usr/bin/xargs"
)

type pkgInstall struct {
	path           string
	choices        json.RawMessage
	allowUntrusted bool
}

func readPkg(args json.RawMessage) (pkgInstall, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(args, &list); err != nil || len(list) == 0 {
		return pkgInstall{}, fmt.Errorf("unexpected arguments")
	}
	var p pkgInstall
	if err := json.Unmarshal(list[0], &p.path); err != nil || p.path == "" {
		return pkgInstall{}, fmt.Errorf("unexpected source")
	}
	for _, opt := range list[1:] {
		var o map[string]json.RawMessage
		if err := json.Unmarshal(opt, &o); err != nil {
			return pkgInstall{}, fmt.Errorf("unexpected options")
		}
		for _, k := range slices.Sorted(maps.Keys(o)) {
			switch k {
			case "allow_untrusted":
				p.allowUntrusted = truthy(o[k])
			case "choices":
				if !emptyChoices(o[k]) {
					p.choices = o[k]
				}
			default:
				return pkgInstall{}, brewError("Unknown key: :%s. Valid keys are: :allow_untrusted, :choices", k)
			}
		}
	}
	return p, nil
}

func truthy(raw json.RawMessage) bool {
	var v any
	return json.Unmarshal(raw, &v) == nil && v != nil && v != false
}

func emptyChoices(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

func (in *Installer) warnAllowUntrusted(pkgs []pkgInstall) {
	if slices.ContainsFunc(pkgs, func(p pkgInstall) bool { return p.allowUntrusted }) {
		in.warnf("Calling `allow_untrusted` in the `pkg` stanza is deprecated! Use a trusted package instead.")
	}
}

func (in *Installer) installPkg(ctx context.Context, token, staged string, p pkgInstall) error {
	in.logf("==> Running installer for %s with `sudo` (which may request your password)...", token)
	path := joinName(staged, p.path)
	if _, err := os.Stat(path); err != nil {
		return missingPkg(staged, path)
	}
	args := []string{"-pkg", path, "-target", "/"}
	if in.Verbose {
		args = append(args, "-verboseR")
	}
	if p.allowUntrusted {
		args = append(args, "-allowUntrusted")
	}
	install := func(args []string) error {
		var env []string
		if u, err := user.Current(); err == nil {
			env = []string{"LOGNAME=" + u.Username, "USER=" + u.Username, "USERNAME=" + u.Username}
		}
		c := in.command(sudo.Root, installerPath, args...)
		c.Env, c.Stdout = env, in.Log
		_, err := in.runner()(ctx, c)
		return err
	}
	if len(p.choices) == 0 {
		return install(args)
	}
	return withChoicesFile(p.choices, func(choicesPath string) error {
		return install(append(args, "-applyChoiceChangesXML", choicesPath))
	})
}

func missingPkg(staged, path string) error {
	rel, err := filepath.Rel(staged, path)
	if err != nil {
		rel = path
	}
	message := fmt.Sprintf("Could not find PKG source file '%s'", rel)
	var found []string
	_ = filepath.WalkDir(staged, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if p == staged {
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(d.Name(), ".pkg") {
			rel, _ := filepath.Rel(staged, p)
			found = append(found, "'"+rel+"'")
		}
		return nil
	})
	if len(found) > 0 {
		message += ", found " + toSentence(found) + " instead"
	}
	return brewError("%s.", message)
}

func toSentence(values []string) string {
	switch len(values) {
	case 0:
		return ""
	case 1:
		return values[0]
	case 2:
		return values[0] + " and " + values[1]
	}
	return strings.Join(values[:len(values)-1], ", ") + " and " + values[len(values)-1]
}

func withChoicesFile(choices json.RawMessage, fn func(path string) error) error {
	body, err := plistDump(choices)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp("", "choices*.xml") //fsx:allow scratch file for installer, as brew's Tempfile
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }() //fsx:allow scratch file for installer
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return fn(f.Name())
}

//go:embed rmdir.sh
var rmdirScript string

// rmdirArgs runs brew's own rmdir.sh when the prefix has Homebrew's library, and the embedded copy otherwise.
func (in *Installer) rmdirArgs() []string {
	libraries := []string{filepath.Join(in.Where.Prefix, "Library")}
	if lib := os.Getenv("HOMEBREW_LIBRARY"); lib != "" {
		libraries = slices.Insert(libraries, 0, lib)
	}
	for _, lib := range libraries {
		script := filepath.Join(lib, "Homebrew/cask/utils/rmdir.sh")
		if fi, err := os.Stat(script); err == nil && fi.Mode().IsRegular() {
			return []string{"-0", "--", script}
		}
	}
	return []string{"-0", "--", "/bin/bash", "-c", rmdirScript, "rmdir.sh"}
}

func (in *Installer) uninstallPkgutil(ctx context.Context, regexes []string) error {
	in.logf("==> Uninstalling packages with `sudo` (which may request your password)...")
	for _, re := range regexes {
		list := in.command(sudo.User, pkgutilPath, "--pkgs="+re)
		list.MustSucceed = false
		out, err := in.capture(ctx, list)
		if err != nil {
			return err
		}
		for _, id := range splitLines(out) {
			in.logf("%s", id)
			if err := in.uninstallPkg(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (in *Installer) pkgRoot(ctx context.Context, id string) (string, error) {
	out, err := in.capture(ctx, in.command(sudo.User, pkgutilPath, "--pkg-info-plist", id))
	if err != nil {
		return "", err
	}
	info, err := plistDictStrings([]byte(out))
	if err != nil {
		return "", fmt.Errorf("%s: pkg info: %w", id, err)
	}
	for _, key := range []string{"volume", "install-location"} {
		if _, ok := info[key]; !ok {
			return "", fmt.Errorf("%s: pkg info: key not found: %q", id, key)
		}
	}
	return joinName(info["volume"], info["install-location"]), nil
}

func (in *Installer) uninstallPkg(ctx context.Context, id string) error {
	out, err := in.capture(ctx, in.command(sudo.User, pkgutilPath, "--files", id))
	if err != nil {
		return err
	}
	var root string
	haveRoot := false
	getRoot := func() (string, error) {
		if !haveRoot {
			r, err := in.pkgRoot(ctx, id)
			if err != nil {
				return "", err
			}
			root, haveRoot = r, true
		}
		return root, nil
	}
	var files, specials, dirs []string
	for _, rel := range splitLines(out) {
		r, err := getRoot()
		if err != nil {
			return err
		}
		path := joinName(r, rel)
		if undeletable(path, in.Where.Home) {
			continue
		}
		switch kind := bomKind(path); {
		case kind.special:
			specials = append(specials, path)
		case kind.file:
			files = append(files, path)
		case kind.dir:
			dirs = append(dirs, path)
		}
	}
	xargs := func(paths []string, args ...string) error {
		c := in.command(sudo.Root, xargsPath, args...)
		c.Input = strings.Join(paths, "\x00")
		_, err := in.runner()(ctx, c)
		return err
	}
	rm := func(paths []string) error { return xargs(paths, "-0", "--", "/bin/rm", "--") }
	rmdir := func(paths []string) error {
		return xargs(paths, in.rmdirArgs()...)
	}
	if len(files) > 0 {
		if err := rm(files); err != nil {
			return err
		}
	}
	if len(specials) > 0 {
		if err := rm(specials); err != nil {
			return err
		}
	}
	if len(dirs) > 0 {
		slices.SortStableFunc(dirs, func(a, b string) int {
			return strings.Count(b, "/") - strings.Count(a, "/")
		})
		if err := rmdir(dirs); err != nil {
			return err
		}
	}
	r, err := getRoot()
	if err != nil {
		return err
	}
	if !undeletable(r, in.Where.Home) {
		if err := rmdir([]string{r}); err != nil {
			return err
		}
	}
	_, err = in.runner()(ctx, in.command(sudo.Root, pkgutilPath, "--forget", id))
	return err
}

type bomEntry struct{ special, file, dir bool }

func bomKind(path string) bomEntry {
	var e bomEntry
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		e.special = true
	}
	fi, err := os.Stat(path)
	if err != nil {
		return e
	}
	e.special = e.special || fi.Mode()&fs.ModeDevice != 0
	e.file = fi.Mode().IsRegular()
	e.dir = fi.IsDir()
	return e
}
