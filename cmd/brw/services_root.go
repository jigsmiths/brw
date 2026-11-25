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

// resolveDomain is brew's System.domain_target, run once before a command uses the domain.
func (e *env) resolveDomain() error {
	e.target = services.Domain(e.uid)
	if e.root() {
		return nil
	}
	overSSH := false
	if os.Getenv("HOMEBREW_SSH_TTY") != "" {
		console, err := e.consoleUID()
		if err != nil {
			return err
		}
		overSSH = console != e.uid
	}
	if !overSSH && os.Getenv("HOMEBREW_SUDO_USER") == "" {
		return nil
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
	e.target = services.UserDomain(e.uid)
	return nil
}

func (e *env) domain() string { return e.target }

func (e *env) domains() []string { return services.Candidates(e.target, e.uid) }

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

func (e *env) checkLoad(t serviceTarget, function string) error {
	switch {
	case e.root() && !t.requiresRoot && e.sudoUser == "":
		e.opoo("`%s` must be run as non-root to start at user login!\nThe service will run as root unless configured otherwise.", t.name)
	case !e.root() && t.requiresRoot:
		e.opoo("`%s` must be run as root to start at system startup!", t.name)
	}
	if e.sudoUser != "" && e.sudoUser != e.user && !e.userExists(e.sudoUser) {
		return fmt.Errorf("Cannot %s `%s` as `%s` is not a user!", function, t.name, e.sudoUser) //nolint:staticcheck // matches brew's error text
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
