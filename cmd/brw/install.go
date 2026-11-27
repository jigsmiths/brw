package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/bottle"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/info"
	"github.com/jigsmiths/brw/internal/install"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/progress"
	"github.com/jigsmiths/brw/internal/rb"
	"github.com/jigsmiths/brw/internal/tap"
	"github.com/jigsmiths/brw/internal/trust"
)

func installCommand(e *env, args []string) error {
	fs := newFlagSet("install")
	var dryRun bool
	prefixArg, journal := changeFlags(e, fs, &dryRun)
	kinds := kindFlags(fs)
	appDir := fs.String("appdir", "", "move apps into this folder instead of /Applications")
	keyboardLayoutDir := fs.String("keyboard-layoutdir", "", "move keyboard layouts into this folder instead of /Library/Keyboard Layouts")
	adopt := fs.Bool("adopt", false, "take over apps already in the app folder")
	head := fs.Bool("HEAD", false, "install a formula's HEAD version, built from its repository")
	trace := traceFlags(fs)
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw install [--formula|--cask] [--HEAD] [--appdir DIR] [--keyboard-layoutdir DIR] [--adopt] [--verbose] [--debug] [--dry-run] NAME [NAME ...]")
	}
	formulaOnly, caskOnly, err := kinds()
	if err != nil {
		return err
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	r, err := e.resolve(context.Background(), prefix, names, formulaOnly, caskOnly)
	if err != nil {
		return err
	}
	failures := r.failures
	if len(r.formulae) > 0 {
		failures = append(failures, installFormulae(e, prefix, *journal, dryRun, *head, r))
	}
	if len(r.casks) > 0 {
		failures = append(failures, installCasks(e, prefix, *journal, *appDir, *keyboardLayoutDir, dryRun, *adopt, *trace, r.casks))
	}
	return errors.Join(failures...)
}

type resolved struct {
	formulae    []string
	casks       []api.Cask
	formulaDeps []string
	idx         *api.Index
	client      *api.Client
	failures    []error
}

func (e *env) resolve(ctx context.Context, prefix string, names []string, formulaOnly, caskOnly bool) (resolved, error) {
	var r resolved
	var err error
	unknown, total := 0, len(names)
	failed, err := e.trustNamed(ctx, prefix, names, formulaOnly, caskOnly)
	if err != nil {
		return r, err
	}
	unknown += len(failed)
	names = slices.DeleteFunc(slices.Clone(names), func(n string) bool { return slices.Contains(failed, n) })
	if !caskOnly {
		if r.idx, r.client, err = e.indexAndClient(ctx, prefix); err != nil {
			return r, err
		}
		if err := e.tapMissing(ctx, prefix, &r, names); err != nil {
			return r, err
		}
	}
	var casks *api.CaskIndex
	loadCasks := func() (*api.CaskIndex, error) {
		if casks == nil {
			var err error
			casks, err = e.caskIndex(ctx, prefix)
			return casks, err
		}
		return casks, nil
	}
	for _, name := range names {
		if r.idx != nil {
			if f, ok := r.idx.Lookup(name); ok {
				if !formulaOnly {
					c, err := e.caskOverTapFormula(name, f, loadCasks)
					if err != nil {
						return r, err
					}
					if c != nil {
						r.casks = append(r.casks, *c)
						continue
					}
				}
				r.formulae = append(r.formulae, name)
				continue
			}
		}
		if !formulaOnly {
			if _, err := loadCasks(); err != nil {
				return r, err
			}
			if tapName, token, ok := tap.Split(name); ok && !strings.HasPrefix(tapName, "homebrew/") {
				c, err := e.tapCask(ctx, prefix, tapName, token)
				if err != nil {
					_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
					unknown++
					continue
				}
				if c != nil {
					r.casks = append(r.casks, *c)
					continue
				}
			} else if c, ok := casks.Lookup(name); ok {
				r.casks = append(r.casks, *c)
				continue
			}
		}
		kind := "formula or cask"
		switch {
		case formulaOnly:
			kind = "formula"
		case caskOnly:
			kind = "cask"
		}
		_, _ = fmt.Fprintf(e.err, "brw: no available %s with the name %q\n", kind, name)
		unknown++
	}
	if unknown > 0 {
		r.failures = append(r.failures, fmt.Errorf("%d of %d names are neither a formula nor a cask", unknown, total))
	}
	return r, nil
}

func (e *env) tapMissing(ctx context.Context, prefix string, r *resolved, names []string) (err error) {
	for missing := untappedTaps(r.idx, prefix, names); len(missing) > 0; missing = untappedTaps(r.idx, prefix, names) {
		for _, name := range missing {
			if _, err := e.ensureTap(ctx, prefix, name); err != nil {
				return err
			}
		}
		if r.idx, r.client, err = e.indexAndClient(ctx, prefix); err != nil {
			return err
		}
	}
	return nil
}

func untappedTaps(idx *api.Index, prefix string, names []string) []string {
	seen := map[string]bool{}
	var missing []string
	queue := slices.Clone(names)
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		if tapName, _, ok := tap.Split(name); ok && !strings.HasPrefix(tapName, "homebrew/") {
			if _, err := os.Stat(tap.Dir(prefix, tapName)); err != nil {
				if !slices.Contains(missing, tapName) {
					missing = append(missing, tapName)
				}
				continue
			}
		}
		if f, ok := idx.Lookup(name); ok {
			queue = append(queue, f.Dependencies...)
		}
	}
	slices.Sort(missing)
	return missing
}

func (e *env) ensureTap(ctx context.Context, prefix, name string) (tap.Tap, error) {
	t := tap.Tap{Name: name, Dir: tap.Dir(prefix, name)}
	if _, err := os.Stat(t.Dir); err == nil {
		return t, nil
	}
	_, _ = fmt.Fprintf(e.err, "==> Tapping %s\n", name)
	t, err := tap.Clone(ctx, prefix, name, e.tapRemoteURL(name))
	if err != nil {
		return t, fmt.Errorf("could not tap %s: %w", name, err)
	}
	return t, nil
}

func (e *env) trustNamed(ctx context.Context, prefix string, names []string, formulaOnly, caskOnly bool) (failed []string, err error) {
	var store *trust.Store
	for _, name := range names {
		tapName, token, ok := tap.Split(name)
		if !ok || strings.HasPrefix(tapName, "homebrew/") {
			continue
		}
		t, err := e.ensureTap(ctx, prefix, tapName)
		if err != nil {
			_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
			failed = append(failed, name)
			continue
		}
		var kind trust.Kind
		switch {
		case !caskOnly && t.HasFormula(token):
			kind = trust.KindFormula
		case !formulaOnly && t.HasCask(token):
			kind = trust.KindCask
		default:
			continue
		}
		if store == nil {
			if store, err = trust.Load(trust.Path()); err != nil {
				return nil, err
			}
		}
		added, err := store.Trust(kind, trust.Reference(tapName, t.Remote())+"/"+strings.ToLower(token))
		if err != nil {
			return nil, err
		}
		if added {
			_, _ = fmt.Fprintf(e.err, "==> Trusted %s %s/%s\n", map[trust.Kind]string{trust.KindFormula: "formula", trust.KindCask: "cask"}[kind], tapName, token)
		}
	}
	return failed, nil
}

func (e *env) tapCask(ctx context.Context, prefix, tapName, token string) (*api.Cask, error) {
	t, err := e.ensureTap(ctx, prefix, tapName)
	if err != nil {
		return nil, err
	}
	tag, err := platform.Tag(e.host)
	if err != nil {
		return nil, err
	}
	c, err := t.Cask(token, tag)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read %s/%s: %w", tapName, token, err)
	}
	store, err := trust.Load(trust.Path())
	if err != nil {
		return nil, err
	}
	added, err := store.Trust(trust.KindCask, trust.Reference(tapName, t.Remote())+"/"+strings.ToLower(c.Token))
	if err != nil {
		return nil, err
	}
	if added {
		_, _ = fmt.Fprintf(e.err, "==> Trusted cask %s/%s\n", tapName, c.Token)
	}
	return &c, nil
}

func (e *env) newPlanner(prefix string, idx *api.Index, client *api.Client, kegs []cellar.Keg) *install.Planner {
	isDir := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && fi.IsDir()
	}
	return &install.Planner{
		Index: idx, Host: e.host, Prefix: prefix, Bottles: client, Kegs: kegs,
		Linked:    func(name string) bool { return isDir(filepath.Join(prefix, "var/homebrew/linked", name)) },
		Optlinked: func(name string) bool { return isDir(filepath.Join(prefix, "opt", name)) },
	}
}

func installFormulae(e *env, prefix, journal string, dryRun, head bool, r resolved) error {
	idx, client := r.idx, r.client
	names := slices.Clone(r.formulae)
	var asDependency []string
	for _, d := range r.formulaDeps {
		if !slices.Contains(names, d) {
			names = append(names, d)
			asDependency = append(asDependency, d)
		}
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return err
	}
	planner := e.newPlanner(prefix, idx, client, kegs)
	planner.AsDependency = asDependency
	if head {
		planner.HeadNames = r.formulae
	}
	plan, planErrs := planner.PlanEach(names)
	for _, err := range planErrs {
		_, _ = fmt.Fprintf(e.err, "brw: %v\n", err)
	}
	var failures []error
	if len(planErrs) > 0 {
		failures = append(failures, fmt.Errorf("%d formulae cannot be installed", len(planErrs)))
	}
	for _, name := range plan.AlreadyInstalled {
		_, _ = fmt.Fprintf(e.err, "brw: warning: %s is already installed\n", name)
	}
	for _, nameVersion := range plan.HeadInstalled {
		name := path.Base(strings.Fields(nameVersion)[0])
		_, _ = fmt.Fprintf(e.err, "brw: warning: %s is already installed and up-to-date.\nTo reinstall HEAD, run:\n  brw reinstall %s\n", nameVersion, name)
	}
	for _, it := range plan.Items {
		for _, w := range it.Warnings {
			_, _ = fmt.Fprintln(e.err, "brw: warning:", w)
		}
	}
	if len(plan.Items) == 0 {
		return errors.Join(failures...)
	}
	if dryRun {
		var b strings.Builder
		fmt.Fprintf(&b, "==> Would install %d %s:\n", len(plan.Items), plural(len(plan.Items), "formula", "formulae"))
		for _, it := range plan.Items {
			fmt.Fprintf(&b, "%s %s\n", it.Formula.Name, it.PkgVersion)
		}
		_, err := fmt.Fprint(e.out, b.String())
		return errors.Join(append(failures, err)...)
	}
	err = withRoot(prefix, journal, func(root *fsx.Root) error {
		return runInstall(e, root, prefix, filepath.Join(client.CacheDir, "downloads"), plan, withBuilds(e, prefix, idx, client))
	})
	return errors.Join(append(failures, err)...)
}

func withBuilds(e *env, prefix string, idx *api.Index, client *api.Client) func(*install.Installer) {
	return func(in *install.Installer) {
		tag, _ := platform.Tag(e.host)
		in.Version = homebrewVersion(prefix)
		in.BuiltOn = buildSystem()
		in.MacVersion = strings.TrimPrefix(in.BuiltOn.OSVersion, "macOS ")
		in.SDK = sdkPath()
		in.LoadSource = func(f *api.Formula) (install.Source, error) {
			file := filepath.Join(tap.Dir(prefix, f.Tap), filepath.FromSlash(f.RubySourcePath))
			code, err := os.ReadFile(file)
			if err != nil {
				return install.Source{}, err
			}
			rf, err := rb.LoadFormula(code, f.Name, tag)
			if err != nil {
				return install.Source{}, err
			}
			return install.Source{Formula: rf, Path: file, Code: code}, nil
		}
		in.DepInfo = func(fullName string) (pkgVersion, license, url, sha string) {
			d, ok := idx.Lookup(fullName)
			if !ok {
				return "", "", "", ""
			}
			pkgVersion, license = pkgVersionOf(d.Versions.Stable, d.Revision), d.License
			bottleTag, ok := platform.Select(e.host, d.BottleTags())
			if !ok {
				return pkgVersion, license, "", ""
			}
			var file api.BottleFile
			var err error
			if d.FromTap() {
				file, err = idx.TapBottle(d.FullName, bottleTag)
			} else {
				var files map[string]api.BottleFile
				if files, err = client.Bottles(map[string]string{d.Name: bottleTag}); err == nil {
					file = files[d.Name]
				}
			}
			if err != nil {
				return pkgVersion, license, "", ""
			}
			return pkgVersion, license, file.URL, file.SHA256
		}
	}
}

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
		Sandbox: poststep.Sandboxed,

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

func sbomCreator() string {
	return sbomCreatorFor(buildInfo())
}

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
		if it.KegOnly && !it.Link {
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
