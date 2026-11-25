package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/services"
)

const rootServiceGroup = "admin"

func (e *env) domain() (string, error) {
	if !e.targetKnown {
		e.target, e.targetErr = e.chooseDomain()
		e.targetKnown = true
	}
	return e.target, e.targetErr
}

func (e *env) domains() ([]string, error) {
	target, err := e.domain()
	if err != nil {
		return nil, err
	}
	return services.Candidates(target, e.uid), nil
}

func (e *env) chooseDomain() (string, error) {
	if e.root() {
		return services.Domain(e.uid), nil
	}
	overSSH := false
	if os.Getenv("HOMEBREW_SSH_TTY") != "" {
		console, err := e.consoleUID()
		if err != nil {
			return "", err
		}
		overSSH = console != e.uid
	}
	if !overSSH && os.Getenv("HOMEBREW_SUDO_USER") == "" {
		return services.Domain(e.uid), nil
	}
	if os.Getenv("HOMEBREW_SERVICES_NO_DOMAIN_WARNING") == "" {
		if overSSH {
			e.opoo("running over SSH without /dev/console ownership, using user/* instead of gui/* domain!")
		} else {
			e.opoo("running through sudo, using user/* instead of gui/* domain!")
		}
		if os.Getenv("HOMEBREW_NO_ENV_HINTS") == "" {
			_, _ = fmt.Fprint(e.err, "Hide this warning by setting `HOMEBREW_SERVICES_NO_DOMAIN_WARNING=1`.\n"+
				"Hide these hints with `HOMEBREW_NO_ENV_HINTS=1` (see `man brew`).\n")
		}
	}
	return services.UserDomain(e.uid), nil
}

func consoleOwner() (int, error) {
	fi, err := os.Stat("/dev/console")
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, errors.New("no owner for /dev/console")
	}
	return int(st.Uid), nil
}

func (e *env) setSudoUser(name string) error {
	if name == "" {
		return nil
	}
	if !e.root() {
		return errors.New("`brw services --sudo-service-user` is supported only when running as root!") //nolint:staticcheck // matches brew's error text
	}
	e.sudoUser = name
	return nil
}

func (e *env) prepareLoad(t serviceTarget, function string) error {
	switch {
	case e.root() && !t.requiresRoot && e.sudoUser == "":
		e.opoo("`%s` must be run as non-root to start at user login!\nThe service will run as root unless configured otherwise.", t.name)
	case !e.root() && t.requiresRoot:
		e.opoo("`%s` must be run as root to start at system startup!", t.name)
	}
	if e.sudoUser != "" && e.sudoUser != e.user && !e.userExists(e.sudoUser) {
		return fmt.Errorf("Cannot %s `%s` as `%s` is not a user!", function, t.name, e.sudoUser) //nolint:staticcheck // matches brew's error text
	}
	return e.makePathDirs(t)
}

func (e *env) makePathDirs(t serviceTarget) error {
	home, _ := os.UserHomeDir()
	dir := func(path string) (string, bool) {
		switch {
		case strings.HasPrefix(path, "/"):
		case path == "~" || strings.HasPrefix(path, "~/"):
			path = home + path[1:]
		default:
			return "", false
		}
		return filepath.Clean(path), true
	}
	var dirs []string
	for _, field := range []string{"working_dir", "root_dir", "input_path", "log_path", "error_log_path"} {
		path, set := t.block.Fields[field]
		if !set {
			continue
		}
		d, ok := dir(path)
		if !ok {
			continue
		}
		if strings.HasSuffix(field, "_path") {
			d = filepath.Dir(d)
		}
		if !slices.Contains(dirs, d) {
			dirs = append(dirs, d)
		}
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil { //fsx:allow directories the service writes to, as brew's path_dirs
			return err
		}
	}
	return nil
}

func (e *env) takeRootOwnership(t serviceTarget, dst string, body []byte) error {
	if !e.root() || e.sudoUser != "" {
		return nil
	}
	if err := e.takeOwnership([]string{dst}, rootServiceGroup, false); err != nil {
		return err
	}
	if !strings.Contains(string(body), "<plist") {
		return nil
	}
	var paths []string
	if program, source := services.Program(body); program != "" {
		located := program
		if !filepath.IsAbs(located) {
			located = filepath.Join(string(filepath.Separator), located)
		}
		if real, err := filepath.EvalSymlinks(located); err == nil {
			paths = append(paths, real, filepath.Dir(real))
		} else {
			e.opoo("%s: the %s does not exist:\n  %s", t.name, source, program)
		}
	}
	paths = append(paths,
		filepath.Join(t.prefix, "opt", t.name),
		filepath.Join(t.prefix, "var", "homebrew", "linked", t.name),
		filepath.Join(t.keg, "bin"),
		filepath.Join(t.keg, "sbin"),
	)
	slices.Sort(paths)
	paths = slices.Compact(paths)
	paths = existing(paths)
	e.opoo("Taking root:%s ownership of some %s paths:\n  %s\n"+
		"This will require manual removal of these paths using `sudo rm` on\n"+
		"brw upgrade/reinstall/uninstall.\n"+
		"Root services will run Homebrew-installed software as root by default. Only use\n"+
		"root services if you trust all users who can write to your Homebrew installation.",
		rootServiceGroup, t.name, strings.Join(paths, "\n  "))
	return e.takeOwnership(paths, rootServiceGroup, true)
}

type ownerChanger struct {
	uid       int
	lookupGid func(group string) (int, error)
}

var rootOwner = ownerChanger{uid: 0, lookupGid: func(name string) (int, error) {
	g, err := user.LookupGroup(name)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(g.Gid)
}}

func (o ownerChanger) apply(paths []string, group string, sticky bool) error {
	gid, err := o.lookupGid(group)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := os.Chown(p, o.uid, gid); err != nil { //fsx:allow root service ownership, as brew's take_root_ownership?
			return err
		}
		if !sticky {
			continue
		}
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if err := os.Chmod(p, fi.Mode()|fs.ModeSticky); err != nil { //fsx:allow root service ownership, as brew's take_root_ownership?
			return err
		}
	}
	return nil
}

func userExists(name string) bool {
	_, err := user.Lookup(name)
	return err == nil
}
