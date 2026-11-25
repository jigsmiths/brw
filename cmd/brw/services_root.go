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

	"github.com/jigsmiths/brw/internal/services"
)

const rootServiceGroup = "admin"

func (e *env) setSudoUser(name string) error {
	if name == "" {
		return nil
	}
	if !e.root() {
		return errors.New("`brw services --sudo-service-user` is supported only when running as root")
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
		return fmt.Errorf("cannot %s `%s` as `%s` is not a user", function, t.name, e.sudoUser)
	}
	return nil
}

// takeRootOwnership hands the service file and the software it runs to root, as brew's take_root_ownership?.
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
	if program, key := services.Program(body); program != "" {
		located := program
		if !filepath.IsAbs(located) {
			located = filepath.Join(string(filepath.Separator), located)
		}
		if real, err := filepath.EvalSymlinks(located); err == nil {
			paths = append(paths, real, filepath.Dir(real))
		} else {
			e.opoo("%s: the %s does not exist:\n  %s", t.name, key, program)
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

func chownToRoot(paths []string, group string, sticky bool) error {
	g, err := user.LookupGroup(group)
	if err != nil {
		return err
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := os.Chown(p, 0, gid); err != nil { //fsx:allow root service ownership, as brew's take_root_ownership?
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
