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
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/progress"
	"github.com/jigsmiths/brw/internal/uninstall"
)

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
			_, _ = fmt.Fprintf(e.err, "brw: warning: %s %s is installed and %s is available; run `brw upgrade %s` to upgrade it\n", c.Token, v, c.Version, c.Token)
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

func upgradeCasks(e *env, prefix, journal string, dryRun bool, tokens []string, o outdated.CaskOptions) (found bool, err error) {
	ctx := context.Background()
	installed, err := caskroom.Installed(prefix)
	if err != nil || (len(installed) == 0 && len(tokens) == 0) {
		return false, err
	}
	idx, err := e.caskIndex(ctx, prefix)
	if err != nil {
		return false, err
	}
	if o.Tag, err = platform.Tag(e.host); err != nil {
		return false, err
	}
	setCaskMachine(&o, prefix)
	hints := o
	if os.Getenv("HOMEBREW_UPGRADE_GREEDY") != "" {
		o.Greedy = true
	}
	byToken := map[string]caskroom.Cask{}
	for _, c := range installed {
		byToken[c.Token] = c
	}

	var stale []outdated.Result
	var failures []error
	if len(tokens) == 0 {
		if stale, err = outdated.CheckCasks(idx, prefix, installed, o); err != nil {
			return false, err
		}
	} else {
		named := o
		named.Greedy = true // brew checks a named cask greedily
		seen := map[string]bool{}
		for _, t := range tokens {
			if c, ok := idx.Lookup(t); ok {
				t = c.Token
			}
			if seen[t] {
				continue
			}
			seen[t] = true
			ic, ok := byToken[t]
			if !ok {
				failures = append(failures, cask.NotInstalled(t))
				continue
			}
			rs, err := outdated.CheckCasks(idx, prefix, []caskroom.Cask{ic}, named)
			if err != nil {
				return false, err
			}
			if len(rs) == 0 {
				_, _ = fmt.Fprintf(e.err, "brw: warning: Not upgrading %s, the latest version is already installed\n", t)
			}
			stale = append(stale, rs...)
		}
	}
	var upgrade, pinned []outdated.Result
	for _, r := range stale {
		if r.Pinned {
			pinned = append(pinned, r)
		} else {
			upgrade = append(upgrade, r)
		}
	}
	// Unnamed, brew leaves pinned casks out without a word, as it does formulae.
	if len(pinned) > 0 && len(tokens) > 0 {
		failures = append(failures, fmt.Errorf("not upgrading %d pinned %s:\n%s", len(pinned),
			plural(len(pinned), "package", "packages"), strings.Join(versionChanges(pinned), "\n")))
	}
	if len(upgrade) == 0 {
		return false, errors.Join(failures...)
	}
	if len(tokens) == 0 {
		printCaskHints(e, idx, upgrade, hints)
	}
	if dryRun {
		_, err := fmt.Fprintf(e.out, "==> Would upgrade %d outdated %s\n%s\n", len(upgrade),
			plural(len(upgrade), "package", "packages"), strings.Join(versionChanges(upgrade), "\n"))
		return true, errors.Join(append(failures, err)...)
	}
	err = withRoot(prefix, journal, func(root *fsx.Root) error {
		return runCaskUpgrades(e, root, prefix, idx, o.Tag, upgrade, byToken)
	})
	return true, errors.Join(append(failures, err)...)
}

// brew's cleanup step is skipped: it cleans brew's own download cache.
func runCaskUpgrades(e *env, root *fsx.Root, prefix string, idx *api.CaskIndex, tag string, upgrade []outdated.Result, installed map[string]caskroom.Cask) error {
	ctx := context.Background()
	client, err := e.apiClient()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	version, _ := buildInfo()
	fetcher := &cask.Fetcher{Dir: filepath.Join(client.CacheDir, "cask-downloads")}
	var display *progress.Display
	if e.tty {
		display = progress.New(e.out, true)
		display.Width = func() int { return terminalWidth(os.Stdout) }
		fetcher.Progress = display.Update
	}
	names := make([]string, len(upgrade))
	for i, r := range upgrade {
		names[i] = r.Name
		if tap := installed[r.Name].Tap; tap != "" && tap != "homebrew/cask" {
			names[i] = tap + "/" + r.Name
		}
	}
	_, _ = fmt.Fprintf(e.out, "==> Fetching downloads for: %s\n", sentence(names))
	var prepared []*cask.PreparedUpgrade
	var ready []outdated.Result
	failed := 0
	for _, r := range upgrade {
		u, closeApps, err := prepareCaskUpgrade(ctx, e, root, prefix, home, version, idx, tag, fetcher, installed[r.Name])
		if err != nil {
			_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
			failed++
			continue
		}
		defer closeApps()
		prepared = append(prepared, u)
		ready = append(ready, r)
	}
	if display != nil {
		display.Close()
	}
	var upgraded []outdated.Result
	for i, u := range prepared {
		if err := u.Run(ctx); err != nil {
			_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
			failed++
			continue
		}
		upgraded = append(upgraded, ready[i])
	}
	// brew sums up only when more than one package changed.
	if len(upgraded) > 1 {
		_, _ = fmt.Fprintf(e.out, "==> Upgraded %d outdated packages\n%s\n", len(upgraded), strings.Join(versionChanges(upgraded), "\n"))
	}
	if failed > 0 {
		return fmt.Errorf("%d of %d casks could not be upgraded", failed, len(upgrade))
	}
	return nil
}

func prepareCaskUpgrade(ctx context.Context, e *env, root *fsx.Root, prefix, home, version string, idx *api.CaskIndex, tag string, fetcher *cask.Fetcher, old caskroom.Cask) (*cask.PreparedUpgrade, func(), error) {
	listed, ok := idx.LookupFrom(old.Tap, old.Token)
	if !ok {
		return nil, nil, fmt.Errorf("%s is not in the cask listing", old.Token)
	}
	c, err := listed.ForTag(tag)
	if err != nil {
		return nil, nil, err
	}
	appDir := caskroom.AppDir(prefix, old.Token)
	apps, err := fsx.Open(appDir, nil)
	if err != nil {
		return nil, nil, err
	}
	in := &cask.Installer{
		Root: root, Apps: apps,
		Where:   cask.Where{Prefix: prefix, AppDir: appDir, Home: home},
		Host:    e.host,
		Fetcher: fetcher,
		Log:     e.out,
		Warn:    e.err,
		Now:     time.Now,
		Version: version,
		APIPath: filepath.Join(brewCache(), "api", "internal", "packages."+tag+".jws.json"),
		Config:  cask.Config{Languages: macLanguages(), ExplicitAppDir: caskroom.ExplicitAppDir(prefix, old.Token)},
	}
	u, err := in.PrepareUpgrade(ctx, old, c)
	if err != nil {
		_ = apps.Close()
		return nil, nil, err
	}
	return u, func() { _ = apps.Close() }, nil
}

func printCaskHints(e *env, idx *api.CaskIndex, upgrade []outdated.Result, o outdated.CaskOptions) {
	if os.Getenv("HOMEBREW_NO_ENV_HINTS") != "" || o.Greedy || len(o.GreedyTokens) > 0 {
		return
	}
	autoUpdates := slices.ContainsFunc(upgrade, func(r outdated.Result) bool {
		c, ok := idx.Lookup(r.Name)
		return ok && c.AutoUpdates
	})
	var b strings.Builder
	if !o.GreedyAutoUpdates && autoUpdates {
		b.WriteString("Homebrew will now attempt to upgrade casks with `auto_updates true`.\n" +
			"Disable this behaviour with `HOMEBREW_NO_UPGRADE_AUTO_UPDATES_CASKS=1`.\n")
	}
	switch {
	case !o.GreedyAutoUpdates && !o.GreedyLatest:
		b.WriteString("Some casks with `auto_updates true` or `version :latest` may still require `--greedy`,\n" +
			"`HOMEBREW_UPGRADE_GREEDY` or `HOMEBREW_UPGRADE_GREEDY_CASKS` to be upgraded.\n")
	case o.GreedyAutoUpdates && !o.GreedyLatest:
		b.WriteString("Casks with `version :latest` will not be upgraded; pass `--greedy-latest` to upgrade them.\n")
	case !o.GreedyAutoUpdates && o.GreedyLatest:
		b.WriteString("Some casks with `auto_updates true` may still require `--greedy-auto-updates` to be upgraded.\n")
	}
	if b.Len() > 0 {
		b.WriteString("Hide these hints with `HOMEBREW_NO_ENV_HINTS=1` (see `man brew`).\n")
	}
	_, _ = fmt.Fprint(e.out, b.String())
}

func versionChanges(rs []outdated.Result) []string {
	sep := "  "
	if len(rs) == 1 {
		sep = " "
	}
	nameWidth, oldWidth := 0, 0
	for _, r := range rs {
		nameWidth = max(nameWidth, len(r.Name))
		oldWidth = max(oldWidth, len(r.InstalledVersions[0]))
	}
	out := make([]string, len(rs))
	for i, r := range rs {
		line := fmt.Sprintf("%-*s%s%-*s -> %s", nameWidth, r.Name, sep, oldWidth, r.InstalledVersions[0], r.CurrentVersion)
		out[i] = strings.TrimRight(line, " ")
	}
	return out
}

func sentence(words []string) string {
	if len(words) < 2 {
		return strings.Join(words, "")
	}
	return strings.Join(words[:len(words)-1], ", ") + " and " + words[len(words)-1]
}

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

func (e *env) checkDependentCasks(named []string, casks, installed []caskroom.Cask) error {
	removing := map[string]bool{}
	for _, c := range casks {
		removing[c.Token] = true
	}
	if len(installed) == len(casks) {
		return nil
	}
	idx, err := e.caskIndex(context.Background(), "")
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

func explicit(arg, abs string) string {
	if arg == "" {
		return ""
	}
	return abs
}

var languageWord = regexp.MustCompile(`[^ \n"(),]+`)

func macLanguages() []string {
	out, err := exec.Command("defaults", "read", "-g", "AppleLanguages").Output()
	if err != nil {
		return nil
	}
	return languageWord.FindAllString(string(out), -1)
}
