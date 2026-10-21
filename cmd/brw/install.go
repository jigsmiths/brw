package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/jigsmiths/brw/internal/bottle"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/info"
	"github.com/jigsmiths/brw/internal/install"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/progress"
)

func installCommand(e *env, args []string) error {
	fs := newFlagSet("install")
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	var dryRun bool
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be installed")
	fs.BoolVar(&dryRun, "n", false, "show what would be installed")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw install [--dry-run] FORMULA [FORMULA ...]")
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	idx, client, err := e.indexAndClient(context.Background())
	if err != nil {
		return err
	}
	kegs, err := cellar.Prefix(prefix).Kegs()
	if err != nil {
		return err
	}
	plan, err := (&install.Planner{
		Index: idx, Host: e.host, Prefix: prefix, Bottles: client, Kegs: kegs,
		Linked: func(name string) bool {
			fi, err := os.Stat(filepath.Join(prefix, "var/homebrew/linked", name))
			return err == nil && fi.IsDir()
		},
	}).Plan(names)
	if err != nil {
		return err
	}
	for _, name := range plan.AlreadyInstalled {
		_, _ = fmt.Fprintf(e.err, "brw: warning: %s is already installed\n", name)
	}
	for _, it := range plan.Items {
		for _, w := range it.Warnings {
			_, _ = fmt.Fprintln(e.err, "brw: warning:", w)
		}
	}
	if len(plan.Items) == 0 {
		return nil
	}
	if dryRun {
		var b strings.Builder
		fmt.Fprintf(&b, "==> Would install %d formula%s:\n", len(plan.Items), map[bool]string{true: "e", false: ""}[len(plan.Items) != 1])
		for _, it := range plan.Items {
			fmt.Fprintf(&b, "%s %s\n", it.Formula.Name, it.PkgVersion)
		}
		_, err := fmt.Fprint(e.out, b.String())
		return err
	}
	return withRoot(prefix, *journal, func(root *fsx.Root) error {
		return runInstall(e, root, prefix, filepath.Join(client.CacheDir, "downloads"), plan)
	})
}

// runInstall carries out a plan and reports what went wrong. opts adjust the
// installer, as an upgrade does.
func runInstall(e *env, root *fsx.Root, prefix, downloads string, plan *install.Plan, opts ...func(*install.Installer)) error {
	hostTag, err := platform.Tag(e.host)
	if err != nil {
		return err
	}
	in := &install.Installer{
		Root: root, Prefix: prefix,
		Fetcher: &bottle.Fetcher{Dir: downloads},
		Linker:  &link.Linker{Root: root, Prefix: prefix},
		APIPath: filepath.Join(brewCache(), "api", "internal", "packages."+hostTag+".jws.json"),
		Log:     e.out,
		User:    os.Getenv("USER"),

		SBOMCreator: sbomCreator(),
	}
	if e.tty {
		display := progress.New(e.out, true)
		display.Width = func() int { return terminalWidth(os.Stdout) }
		defer display.Close()
		in.Fetcher.Progress = display.Update
	}
	for _, opt := range opts {
		opt(in)
	}
	results, err := in.Install(context.Background(), plan)

	notLinked, postFailed, failed := 0, 0, 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			_, _ = fmt.Fprintf(e.err, "brw: %v\n", r.Err)
			continue
		}
		if r.LinkErr != nil {
			notLinked++
			_, _ = fmt.Fprintf(e.err, "brw: %s %s was poured but could not be linked: %v\n", r.Name, r.Version, r.LinkErr)
		}
		if r.PostInstallErr != nil {
			postFailed++
			_, _ = fmt.Fprintf(e.err, "brw: the post_install step of %s did not complete: %v\n", r.Name, r.PostInstallErr)
		}
	}
	printCaveats(e, plan, results, prefix)
	if err != nil {
		return err
	}
	var problems []error
	if failed > 0 {
		problems = append(problems, fmt.Errorf("%d of %d formulae could not be installed", failed, len(results)))
	}
	if notLinked > 0 {
		problems = append(problems, fmt.Errorf("%d of %d formulae installed but not linked", notLinked, len(results)))
	}
	if postFailed > 0 {
		problems = append(problems, fmt.Errorf("%d of %d formulae installed but their post_install did not complete", postFailed, len(results)))
	}
	return errors.Join(problems...)
}

// sbomCreator names brw in the SBOMs of what it installs.
func sbomCreator() string {
	return sbomCreatorFor(buildInfo())
}

// sbomCreatorFor adds the version brw was installed as, or else the start of the
// commit it was built from, when there is one.
func sbomCreatorFor(version, revision string) string {
	const creator = "Tool: https://github.com/jigsmiths/brw"
	switch {
	case version != "" && version != "(devel)":
		return creator + "@" + version
	case len(revision) >= 7:
		return creator + "@" + revision[:7]
	}
	return creator
}

// printCaveats shows the keg-only notices and the caveats of what was installed.
func printCaveats(e *env, plan *install.Plan, results []install.Result, prefix string) {
	installed := map[string]bool{}
	for _, r := range results {
		installed[r.Name] = r.Err == nil
	}
	header := false
	for _, it := range plan.Items {
		if !installed[it.Formula.Name] {
			continue
		}
		text := info.Caveats(it.Formula, prefix)
		if it.KegOnly && (it.Linked == nil || !*it.Linked) {
			if text != "" {
				text += "\n"
			}
			text += fmt.Sprintf("%s is keg-only, which means it was not symlinked into %s\n", it.Formula.Name, prefix)
		}
		if text == "" {
			continue
		}
		if !header {
			_, _ = fmt.Fprintln(e.out, "==> Caveats")
			header = true
		}
		_, _ = fmt.Fprintf(e.out, "==> %s\n%s", it.Formula.Name, text)
	}
}

// brewCache is where brew keeps its downloads and API data.
func brewCache() string {
	if d := os.Getenv("HOMEBREW_CACHE"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	switch {
	case runtime.GOOS == "darwin":
		return filepath.Join(home, "Library", "Caches", "Homebrew")
	case os.Getenv("XDG_CACHE_HOME") != "":
		return filepath.Join(os.Getenv("XDG_CACHE_HOME"), "Homebrew")
	}
	return filepath.Join(home, ".cache", "Homebrew")
}
