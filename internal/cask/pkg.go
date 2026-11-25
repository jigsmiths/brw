package cask

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
)

const (
	installerPath = "/usr/sbin/installer"
	pkgutilPath   = "/usr/sbin/pkgutil"
	sudoPath      = "/usr/bin/sudo"
	envPath       = "/usr/bin/env"
	xargsPath     = "/usr/bin/xargs"
)

// Command is one process to run; Name and Args already carry any sudo prefix.
type Command struct {
	Name  string
	Args  []string
	Stdin []byte
	// Tee also receives the process's stdout as it is written.
	Tee io.Writer
}

type Runner func(ctx context.Context, c Command) ([]byte, error)

func execRun(ctx context.Context, c Command) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	if c.Tee != nil {
		cmd.Stdout = io.MultiWriter(&out, c.Tee)
	}
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %w: %s", c.Name, strings.Join(c.Args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return out.Bytes(), nil
}

func (in *Installer) run(ctx context.Context, c Command) ([]byte, error) {
	if in.Run != nil {
		return in.Run(ctx, c)
	}
	return execRun(ctx, c)
}

var geteuid = os.Geteuid

const rootRefusal = `Running Homebrew as root is extremely dangerous and no longer supported.
As Homebrew does not drop privileges on installation you would be giving all
build scripts full access to your system.`

func refuseRoot() error {
	if geteuid() == 0 {
		return brewError("%s", rootRefusal)
	}
	return nil
}

// brewCommand wraps exe the way brew's SystemCommand does, with sudo_as_root when sudo is set.
func brewCommand(sudo bool, env []string, exe string, args ...string) Command {
	if !sudo {
		return Command{Name: envPath, Args: append([]string{exe}, args...)}
	}
	pre := []string{"-u", "root"}
	if _, ok := os.LookupEnv("SUDO_ASKPASS"); ok {
		pre = append(pre, "-A")
	}
	pre = append(pre, "-E")
	pre = append(pre, env...)
	pre = append(pre, "--", exe)
	return Command{Name: sudoPath, Args: append(pre, args...)}
}

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
	if p.allowUntrusted {
		args = append(args, "-allowUntrusted")
	}
	install := func(args []string) error {
		var env []string
		if u, err := user.Current(); err == nil {
			env = []string{"LOGNAME=" + u.Username, "USER=" + u.Username, "USERNAME=" + u.Username}
		}
		c := brewCommand(true, env, installerPath, args...)
		c.Tee = in.Log
		_, err := in.run(ctx, c)
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

// rmdirScript is brew's cask/utils/rmdir.sh; unlike brew, it runs through bash -c as brw has no Homebrew library to read it from.
const rmdirScript = `set -euo pipefail

# Try removing as many empty directories as possible with a single
# ` + "`rmdir`" + ` call to avoid or at least speed up the loop below.
if rmdir -- "${@}" &>/dev/null
then
  exit
fi

for path in "${@}"
do
  symlink=true
  [[ -L "${path}" ]] || symlink=false

  directory=false
  if [[ -d "${path}" ]]
  then
    directory=true

    if [[ -e "${path}/.DS_Store" ]]
    then
      /bin/rm -- "${path}/.DS_Store"
    fi

    # Some packages leave broken symlinks around; we clean them out before
    # attempting to ` + "`rmdir`" + ` to prevent extra cruft from accumulating.
    /usr/bin/find "${path}" -mindepth 1 -maxdepth 1 -type l ! -exec test -e {} \; -delete || true
  elif ! ${symlink} && [[ ! -e "${path}" ]]
  then
    # Skip paths that don't exists and aren't a broken symlink.
    continue
  fi

  if ${symlink}
  then
    # Delete directory symlink.
    /bin/rm -- "${path}"
  elif ${directory}
  then
    # Delete directory if empty.
    /usr/bin/find "${path}" -maxdepth 0 -type d -empty -exec rmdir -- {} \;
  else
    # Try ` + "`rmdir`" + ` anyways to show a proper error.
    rmdir -- "${path}"
  fi
done
`

func (in *Installer) uninstallPkgutil(ctx context.Context, regexes []string) error {
	in.logf("==> Uninstalling packages with `sudo` (which may request your password)...")
	for _, re := range regexes {
		out, _ := in.run(ctx, brewCommand(false, nil, pkgutilPath, "--pkgs="+re))
		for _, id := range splitLines(string(out)) {
			in.logf("%s", id)
			if err := in.uninstallPkg(ctx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// splitLines is Ruby's split("\n"), which drops trailing empty fields.
func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func (in *Installer) pkgRoot(ctx context.Context, id string) (string, error) {
	out, err := in.run(ctx, brewCommand(false, nil, pkgutilPath, "--pkg-info-plist", id))
	if err != nil {
		return "", err
	}
	info, err := plistDictStrings(out)
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
	out, err := in.run(ctx, brewCommand(false, nil, pkgutilPath, "--files", id))
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
	for _, rel := range splitLines(string(out)) {
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
	rm := func(paths []string) error {
		_, err := in.run(ctx, withInput(brewCommand(true, nil, xargsPath, "-0", "--", "/bin/rm", "--"), paths))
		return err
	}
	rmdir := func(paths []string) error {
		_, err := in.run(ctx, withInput(brewCommand(true, nil, xargsPath, "-0", "--", "/bin/bash", "-c", rmdirScript, "rmdir.sh"), paths))
		return err
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
	_, err = in.run(ctx, brewCommand(true, nil, pkgutilPath, "--forget", id))
	return err
}

func withInput(c Command, paths []string) Command {
	c.Stdin = []byte(strings.Join(paths, "\x00"))
	return c
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
