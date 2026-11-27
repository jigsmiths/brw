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
	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/progress"
	"github.com/jigsmiths/brw/internal/uninstall"
)

func installCasks(e *env, prefix, journal, appDirArg string, dryRun, adopt bool, trace tracing, casks []api.Cask) error {
	e.setTracing(trace)
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
	caskIdx, err := e.caskIndex(context.Background(), prefix)
	if err != nil {
		return err
	}
	if dryRun {
		var b strings.Builder
		fmt.Fprintf(&b, "==> Would install %d %s:\n", len(todo), plural(len(todo), "cask", "casks"))
		for _, c := range todo {
			fmt.Fprintf(&b, "%s %s\n", c.Token, c.Version)
		}
		var errs []error
		for _, c := range todo {
			names, err := e.dryRunDependencies(context.Background(), prefix, tag, caskIdx, c)
			if err != nil {
				_, _ = fmt.Fprintf(e.err, "brw: %s: %v\n", c.Token, err)
				errs = append(errs, err)
				continue
			}
			if len(names) > 0 {
				fmt.Fprintf(&b, "==> Would install %d %s for %s:\n%s\n", len(names), plural(len(names), "dependency", "dependencies"), c.Token, strings.Join(names, " "))
			}
		}
		_, err := fmt.Fprint(e.out, b.String())
		return errors.Join(append(errs, err)...)
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
		in := e.caskInstaller(root, prefix, appDir, home, caskDownloads(client), trace)
		in.Apps = apps
		in.Fetcher = &cask.Fetcher{Dir: caskDownloads(client)}
		in.Now = time.Now
		in.Version = version
		in.Adopt = adopt
		in.RuntimeDeps = e.runtimeDepsFor(context.Background(), prefix, tag, caskIdx, nil)
		in.APIPath = filepath.Join(brewCache(), "api", "internal", "packages."+tag+".jws.json")
		in.Config = cask.Config{Languages: macLanguages(), ExplicitAppDir: explicit(appDirArg, appDir)}
		in.SatisfyDependencies = func(ctx context.Context, c api.Cask) error {
			return e.satisfyDependencies(ctx, prefix, journal, in, appDir, in.Config.ExplicitAppDir, tag, caskIdx, c)
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

func resolveCasks(e *env, prefix, journal string, dryRun bool, trace tracing, tokens []string, o outdated.CaskOptions) *upgradeStep {
	ctx := context.Background()
	installed, err := caskroom.Installed(prefix)
	if err != nil || (len(installed) == 0 && len(tokens) == 0) {
		return failedStep(err)
	}
	idx, err := e.caskIndex(ctx, prefix)
	if err != nil {
		return failedStep(err)
	}
	if o.Tag, err = platform.Tag(e.host); err != nil {
		return failedStep(err)
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
			return failedStep(err)
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
				return failedStep(err)
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
	var left []outdated.Result
	if len(pinned) > 0 && len(tokens) > 0 {
		failures = append(failures, fmt.Errorf("not upgrading %d pinned %s:\n%s", len(pinned),
			plural(len(pinned), "package", "packages"), strings.Join(versionChanges(pinned), "\n")))
	} else {
		left = pinned
	}
	return &upgradeStep{upgrade: upgrade, pinned: left, failures: failures, run: func() ([]string, error) {
		if len(tokens) == 0 {
			printCaskHints(e, idx, upgrade, hints)
		}
		if dryRun {
			_, err := fmt.Fprintf(e.out, "==> Would upgrade %d outdated %s\n%s\n", len(upgrade),
				plural(len(upgrade), "package", "packages"), strings.Join(versionChanges(upgrade), "\n"))
			return nil, err
		}
		var upgraded []string
		err := withRoot(prefix, journal, func(root *fsx.Root) (err error) {
			upgraded, err = runCaskUpgrades(e, root, prefix, journal, trace, idx, o.Tag, upgrade, byToken)
			return err
		})
		return upgraded, err
	}}
}

// brew's cleanup step is skipped: it cleans brew's own download cache.
func runCaskUpgrades(e *env, root *fsx.Root, prefix, journal string, trace tracing, idx *api.CaskIndex, tag string, upgrade []outdated.Result, installed map[string]caskroom.Cask) ([]string, error) {
	ctx := context.Background()
	client, err := e.apiClient()
	if err != nil {
		return nil, err
	}
	home, _ := os.UserHomeDir()
	version, _ := buildInfo()
	fetcher := &cask.Fetcher{Dir: caskDownloads(client)}
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
	var failed []string
	for _, r := range upgrade {
		u, closeApps, err := prepareCaskUpgrade(ctx, e, root, prefix, journal, home, version, trace, idx, tag, fetcher, installed[r.Name])
		if err != nil {
			_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
			failed = append(failed, r.Name)
			continue
		}
		defer closeApps()
		prepared = append(prepared, u)
		ready = append(ready, r)
	}
	if display != nil {
		display.Close()
	}
	var upgraded []string
	for i, u := range prepared {
		if err := u.Run(ctx); err != nil {
			_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
			failed = append(failed, ready[i].Name)
			continue
		}
		upgraded = append(upgraded, ready[i].Name)
	}
	if len(failed) > 0 {
		return upgraded, fmt.Errorf("%d of %d casks could not be upgraded: %s", len(failed), len(upgrade), strings.Join(failed, ", "))
	}
	return upgraded, nil
}

const defaultAppDir = "/Applications"

func prepareCaskUpgrade(ctx context.Context, e *env, root *fsx.Root, prefix, journal, home, version string, trace tracing, idx *api.CaskIndex, tag string, fetcher *cask.Fetcher, old caskroom.Cask) (*cask.PreparedUpgrade, func(), error) {
	e.setTracing(trace)
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
	in := e.caskInstaller(root, prefix, appDir, home, fetcher.Dir, trace)
	in.Apps = apps
	in.Fetcher = fetcher
	in.Now = time.Now
	in.Version = version
	in.APIPath = filepath.Join(brewCache(), "api", "internal", "packages."+tag+".jws.json")
	in.Config = cask.Config{Languages: macLanguages(), ExplicitAppDir: caskroom.ExplicitAppDir(prefix, old.Token)}
	in.RuntimeDeps = e.runtimeDepsFor(ctx, prefix, tag, idx, nil)
	in.SatisfyDependencies = func(ctx context.Context, c api.Cask) error {
		return e.satisfyDependencies(ctx, prefix, journal, in, defaultAppDir, "", tag, idx, c)
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
		b.WriteString("brw will now attempt to upgrade casks with `auto_updates true`.\n" +
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

func uninstallCasks(e *env, root *fsx.Root, prefix string, named []string, casks, installed []caskroom.Cask, dryRun, force, ignoreDeps bool, trace tracing) error {
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
		errs = append(errs, uninstallCask(e, root, prefix, home, c, force, trace))
	}
	return errors.Join(errs...)
}

// caskInstaller sets what every cask operation needs, including uninstall, which downloads nothing.
func (e *env) caskInstaller(root *fsx.Root, prefix, appDir, home, cacheDir string, trace tracing) *cask.Installer {
	return &cask.Installer{
		Root:        root,
		Where:       cask.NewWhere(prefix, appDir, home),
		Host:        e.host,
		Sandbox:     poststep.Sandboxed,
		CacheDir:    cacheDir,
		Log:         e.out,
		Warn:        e.err,
		Verbose:     trace.verbose,
		Debug:       trace.debug,
		Run:         e.sudoRun,
		FindService: e.findService,
	}
}

func caskDownloads(client *api.Client) string {
	return filepath.Join(client.CacheDir, "cask-downloads")
}

func uninstallCask(e *env, root *fsx.Root, prefix, home string, c caskroom.Cask, force bool, trace tracing) error {
	appDir := caskroom.AppDir(prefix, c.Token)
	client, err := e.apiClient()
	if err != nil {
		return err
	}
	in := e.caskInstaller(root, prefix, appDir, home, caskDownloads(client), trace)
	e.setTracing(trace)
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
