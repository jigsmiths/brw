package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cask"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/install"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/progress"
	"github.com/jigsmiths/brw/internal/uninstall"
)

// installCasks installs the casks that are not installed yet. brw does not
// upgrade casks, so an installed one is left with a warning.
func installCasks(e *env, prefix, journal, appDirArg string, dryRun, adopt bool, casks []api.Cask) error {
	tag, err := platform.Tag(e.host)
	if err != nil {
		return err
	}
	installed, err := caskroom.Installed(prefix)
	if err != nil {
		return err
	}
	versions := map[string]string{}
	for _, c := range installed {
		versions[c.Token] = c.Version
	}
	var todo []api.Cask
	for _, c := range casks {
		c, err := c.ForTag(tag)
		if err != nil {
			return err
		}
		switch v, ok := versions[c.Token]; {
		case !ok:
			todo = append(todo, c)
		case v == c.Version:
			_, _ = fmt.Fprintf(e.err, "brw: warning: Not upgrading %s, the latest version is already installed\n", c.Token)
		default:
			_, _ = fmt.Fprintf(e.err, "brw: warning: %s %s is installed and %s is available; brw cannot upgrade casks yet\n", c.Token, v, c.Version)
		}
	}
	if len(todo) == 0 {
		return nil
	}
	if dryRun {
		var b strings.Builder
		fmt.Fprintf(&b, "==> Would install %d %s:\n", len(todo), plural(len(todo), "cask", "casks"))
		for _, c := range todo {
			fmt.Fprintf(&b, "%s %s\n", c.Token, c.Version)
		}
		_, err := fmt.Fprint(e.out, b.String())
		return err
	}

	appDir := "/Applications"
	if appDirArg != "" {
		if appDir, err = filepath.Abs(appDirArg); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(appDir, 0o755); err != nil { //fsx:allow the applications folder, outside the prefix
		return err
	}
	client, err := e.apiClient()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	version, _ := buildInfo()
	return withRoot(prefix, journal, func(root *fsx.Root) error {
		apps, err := fsx.Open(appDir, nil)
		if err != nil {
			return err
		}
		defer func() { _ = apps.Close() }()
		if err := install.MakeStandardDirs(root, e.host.OS); err != nil {
			return err
		}
		in := &cask.Installer{
			Root: root, Apps: apps,
			Where:   cask.Where{Prefix: prefix, AppDir: appDir, Home: home},
			Host:    e.host,
			Fetcher: &cask.Fetcher{Dir: filepath.Join(client.CacheDir, "cask-downloads")},
			Log:     e.out,
			Warn:    e.err,
			Now:     time.Now,
			Version: version,
			Adopt:   adopt,
			APIPath: filepath.Join(brewCache(), "api", "internal", "packages."+tag+".jws.json"),
			Config:  cask.Config{Languages: macLanguages(), ExplicitAppDir: explicit(appDirArg, appDir)},
		}
		if e.tty {
			display := progress.New(e.out, true)
			display.Width = func() int { return terminalWidth(os.Stdout) }
			defer display.Close()
			in.Fetcher.Progress = display.Update
		}
		failed := 0
		for _, c := range todo {
			if err := in.Install(context.Background(), c); err != nil {
				_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
				failed++
			}
		}
		if failed > 0 {
			return fmt.Errorf("%d of %d casks could not be installed", failed, len(todo))
		}
		return nil
	})
}

// uninstallCasks removes installed casks as brew does. It refuses brw's own
// cask unless forced, and a cask another installed cask needs.
func uninstallCasks(e *env, root *fsx.Root, prefix string, named []string, casks, installed []caskroom.Cask, dryRun, force, ignoreDeps bool) error {
	if !force && slices.ContainsFunc(casks, func(c caskroom.Cask) bool { return c.Token == "brw" }) {
		return errors.New("refusing to uninstall the brw cask, which is brw itself; use --force to remove it")
	}
	if !ignoreDeps {
		if err := e.checkDependentCasks(named, casks, installed); err != nil {
			return err
		}
	}
	var todo []caskroom.Cask
	var errs []error
	for _, c := range casks {
		if _, pinned := caskroom.Pinned(prefix, c.Token); pinned {
			if !force {
				errs = append(errs, cask.IsPinned(c.Token))
				continue
			}
			if !dryRun {
				if err := root.Remove(filepath.Join("var/homebrew/pinned_casks", c.Token)); err != nil {
					return err
				}
			}
		}
		todo = append(todo, c)
	}
	if dryRun {
		if len(todo) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "==> Would uninstall %d %s:\n", len(todo), plural(len(todo), "cask", "casks"))
			for _, c := range todo {
				fmt.Fprintf(&b, "%s %s\n", c.Token, c.Version)
			}
			if _, err := fmt.Fprint(e.out, b.String()); err != nil {
				return err
			}
		}
		return errors.Join(errs...)
	}
	home, _ := os.UserHomeDir()
	for _, c := range todo {
		errs = append(errs, uninstallCask(e, root, prefix, home, c, force))
	}
	return errors.Join(errs...)
}

func uninstallCask(e *env, root *fsx.Root, prefix, home string, c caskroom.Cask, force bool) error {
	appDir := caskroom.AppDir(prefix, c.Token)
	in := &cask.Installer{
		Root:  root,
		Where: cask.Where{Prefix: prefix, AppDir: appDir, Home: home},
		Host:  e.host,
		Log:   e.out, Warn: e.err,
	}
	// A missing app folder holds no app to move back.
	if _, err := os.Stat(appDir); err == nil {
		apps, err := fsx.Open(appDir, nil)
		if err != nil {
			return err
		}
		defer func() { _ = apps.Close() }()
		in.Apps = apps
	}
	return in.Uninstall(context.Background(), c.Token, c.Version, force)
}

// checkDependentCasks refuses to remove a cask that another installed cask
// depends on. Without the cask listing, there is nothing to check against.
func (e *env) checkDependentCasks(named []string, casks, installed []caskroom.Cask) error {
	removing := map[string]bool{}
	for _, c := range casks {
		removing[c.Token] = true
	}
	if len(installed) == len(casks) {
		return nil
	}
	idx, err := e.caskIndex(context.Background())
	if err != nil {
		_, _ = fmt.Fprintf(e.err, "brw: warning: not checking for casks that need these: %v\n", err)
		return nil
	}
	var required, dependents []string
	for _, c := range installed {
		if removing[c.Token] {
			continue
		}
		def, ok := idx.Lookup(c.Token)
		if !ok {
			continue
		}
		found := false
		for _, d := range def.DependsOn.Cask {
			if removing[d] {
				found = true
				if !slices.Contains(required, d) {
					required = append(required, d)
				}
			}
		}
		if found {
			dependents = append(dependents, c.Token)
		}
	}
	if len(dependents) == 0 {
		return nil
	}
	return &uninstall.DependentsError{Kegs: required, Dependents: dependents, Named: named}
}

// explicit is the app folder to record as given on the command line, or "".
func explicit(arg, abs string) string {
	if arg == "" {
		return ""
	}
	return abs
}

var languageWord = regexp.MustCompile(`[^ \n"(),]+`)

// macLanguages are the user's preferred languages, read as brew reads them.
func macLanguages() []string {
	out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output()
	if err != nil {
		return nil
	}
	return languageWord.FindAllString(string(out), -1)
}
