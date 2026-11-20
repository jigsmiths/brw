package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cask"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/install"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/progress"
)

// installCasks installs the casks that are not installed yet. brw does not
// upgrade casks, so an installed one is left with a warning.
func installCasks(e *env, prefix, journal, appDirArg string, dryRun bool, casks []api.Cask) error {
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
		fmt.Fprintf(&b, "==> Would install %d cask%s:\n", len(todo), map[bool]string{true: "s", false: ""}[len(todo) != 1])
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
