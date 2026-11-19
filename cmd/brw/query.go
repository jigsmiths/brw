package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/deps"
	"github.com/jigsmiths/brw/internal/info"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/search"
)

func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// parse reads flags that may come before or after the positional arguments.
func parse(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func prefixFlag(e *env, fs *flag.FlagSet) *string {
	return fs.String("prefix", defaultPrefix(e.host), "Homebrew prefix")
}

// apiClient returns the formula API client, warning on stderr when it falls
// back to the cache.
func (e *env) apiClient() (*api.Client, error) {
	c := e.client
	if c == nil {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, err
		}
		c = &api.Client{URL: api.DefaultURL, CaskURL: api.DefaultCaskURL, CacheDir: filepath.Join(dir, "brw")}
	}
	warned := *c
	warned.Warn = func(msg string) { _, _ = fmt.Fprintln(e.err, "brw: warning:", msg) }
	return &warned, nil
}

// index loads the formula listing, using the cache when offline.
func (e *env) index(ctx context.Context) (*api.Index, error) {
	idx, _, err := e.indexAndClient(ctx)
	return idx, err
}

func (e *env) indexAndClient(ctx context.Context) (*api.Index, *api.Client, error) {
	c, err := e.apiClient()
	if err != nil {
		return nil, nil, err
	}
	formulae, err := c.Formulae(ctx)
	if err != nil {
		return nil, nil, err
	}
	return api.NewIndex(formulae), c, nil
}

func list(e *env, args []string) error {
	fs := newFlagSet("list")
	prefix := prefixFlag(e, fs)
	versions := fs.Bool("versions", false, "show installed versions")
	pinned := fs.Bool("pinned", false, "list only pinned formulae")
	var formulaOnly, caskOnly bool
	fs.BoolVar(&formulaOnly, "formula", false, "list only formulae")
	fs.BoolVar(&formulaOnly, "formulae", false, "list only formulae")
	fs.BoolVar(&caskOnly, "cask", false, "list only casks")
	fs.BoolVar(&caskOnly, "casks", false, "list only casks")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if formulaOnly && caskOnly {
		return errors.New("--formula and --cask are mutually exclusive")
	}
	if *pinned {
		if len(rest) > 0 {
			return errors.New("--pinned with formula names is not supported yet")
		}
		if caskOnly {
			return errors.New("--pinned with --cask is not supported yet")
		}
		out, err := pinnedList(*prefix, *versions)
		if err == nil {
			_, err = io.WriteString(e.out, out)
		}
		return err
	}

	// Like brew, a terminal gets a heading over each kind unless one was asked for.
	headings := e.tty && !formulaOnly && !caskOnly
	var b strings.Builder
	if !caskOnly {
		if *versions {
			kegs, err := e.kegs(*prefix)
			if err != nil {
				return err
			}
			b.WriteString(formatVersions(kegs))
		} else {
			names, err := cellar.Prefix(*prefix).Names()
			if err != nil {
				return err
			}
			if len(names) > 0 && headings {
				b.WriteString("==> Formulae\n" + lines(names) + "\n")
			} else {
				b.WriteString(lines(names))
			}
		}
	}
	if !formulaOnly {
		if *versions {
			casks, err := caskroom.Installed(*prefix)
			if err != nil {
				return err
			}
			for _, c := range casks {
				b.WriteString(c.Token + " " + c.Version + "\n")
			}
		} else {
			names, broken, err := caskroom.Names(*prefix)
			if err != nil {
				return err
			}
			if len(names) > 0 && headings {
				b.WriteString("==> Casks\n")
			}
			b.WriteString(lines(names))
			if len(broken) > 0 {
				_, _ = fmt.Fprintf(e.err, "brw: warning: broken Caskroom symlinks: %s\n", strings.Join(broken, ", "))
			}
		}
	}
	_, err = io.WriteString(e.out, b.String())
	return err
}

// formatVersions matches the output of brew list --versions.
func formatVersions(kegs []cellar.Keg) string {
	var b strings.Builder
	for i, k := range kegs {
		if i == 0 || kegs[i-1].Name != k.Name {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(k.Name)
		}
		b.WriteString(" " + k.Version)
	}
	if len(kegs) > 0 {
		b.WriteByte('\n')
	}
	return b.String()
}

// caskIndex loads the cask listing, using the cache when offline.
func (e *env) caskIndex(ctx context.Context) (*api.CaskIndex, error) {
	c, err := e.apiClient()
	if err != nil {
		return nil, err
	}
	casks, err := c.Casks(ctx)
	if err != nil {
		return nil, err
	}
	return api.NewCaskIndex(casks), nil
}

func searchCommand(e *env, args []string) error {
	fs := newFlagSet("search")
	desc := fs.Bool("desc", false, "search descriptions")
	var formulaOnly, caskOnly bool
	fs.BoolVar(&formulaOnly, "formula", false, "only formulae")
	fs.BoolVar(&formulaOnly, "formulae", false, "only formulae")
	fs.BoolVar(&caskOnly, "cask", false, "only casks")
	fs.BoolVar(&caskOnly, "casks", false, "only casks")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: brw search [--formula|--cask] [--desc] TEXT|/REGEX/")
	}
	if formulaOnly && caskOnly {
		return errors.New("--formula and --cask are mutually exclusive")
	}
	query := pos[0]
	ctx := context.Background()
	var idx *api.Index
	if !caskOnly {
		if idx, err = e.index(ctx); err != nil {
			return err
		}
	}
	var casks *api.CaskIndex
	if !formulaOnly {
		if casks, err = e.caskIndex(ctx); err != nil {
			return err
		}
	}

	var b strings.Builder
	if *desc {
		// Brew heads each kind it searched, even when nothing matched.
		if idx != nil {
			matches, err := search.Descriptions(idx, query)
			if err != nil {
				return err
			}
			b.WriteString("==> Formulae\n")
			for _, m := range matches {
				b.WriteString(m.Name + ": " + m.Desc + "\n")
			}
		}
		if casks != nil {
			matches, err := search.CaskDescriptions(casks, query)
			if err != nil {
				return err
			}
			if idx != nil {
				b.WriteByte('\n')
			}
			b.WriteString("==> Casks\n")
			for _, m := range matches {
				b.WriteString(m.Token + ": ")
				if m.Names != "" {
					b.WriteString("(" + m.Names + ") ")
				}
				b.WriteString(m.Desc + "\n")
			}
		}
		_, err = io.WriteString(e.out, b.String())
		return err
	}

	var names, tokens []string
	if idx != nil {
		if names, err = search.Names(idx, query); err != nil {
			return err
		}
	}
	if casks != nil {
		if tokens, err = search.CaskTokens(casks, query); err != nil {
			return err
		}
	}
	if len(names)+len(tokens) == 0 {
		return fmt.Errorf("no formulae or casks found for %q", query)
	}
	if len(names) > 0 {
		if e.tty {
			b.WriteString("==> Formulae\n")
		}
		b.WriteString(lines(names))
	}
	if len(names) > 0 && len(tokens) > 0 {
		b.WriteByte('\n')
	}
	if len(tokens) > 0 {
		if e.tty {
			b.WriteString("==> Casks\n")
		}
		b.WriteString(lines(tokens))
	}
	_, err = io.WriteString(e.out, b.String())
	return err
}

func depsCommand(e *env, args []string) error {
	fs := newFlagSet("deps")
	prefix := prefixFlag(e, fs)
	tree := fs.Bool("tree", false, "show the declared dependency tree")
	direct := fs.Bool("1", false, "only direct declared dependencies")
	installed := fs.Bool("installed", false, "use the installed receipt")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: brw deps [--installed] [--tree] [--1] FORMULA")
	}
	name := pos[0]
	kegs, err := e.kegs(*prefix)
	if err != nil {
		return err
	}

	var out string
	if *installed || (!*tree && !*direct && hasKeg(kegs, name)) {
		names, err := deps.Installed(kegs, name)
		if err != nil {
			return err
		}
		out = lines(names)
	} else {
		idx, err := e.index(context.Background())
		if err != nil {
			return err
		}
		r, err := deps.NewResolver(idx, e.host)
		if err != nil {
			return err
		}
		switch {
		case *tree:
			if out, err = r.Tree(name); err != nil {
				return err
			}
			out += "\n"
		case *direct:
			names, err := r.Direct(name)
			if err != nil {
				return err
			}
			out = lines(names)
		default:
			names, err := r.Recursive(name)
			if err != nil {
				return err
			}
			out = lines(names)
		}
	}
	_, err = io.WriteString(e.out, out)
	return err
}

func usesCommand(e *env, args []string) error {
	fs := newFlagSet("uses")
	prefix := prefixFlag(e, fs)
	installed := fs.Bool("installed", false, "only installed formulae")
	recursive := fs.Bool("recursive", false, "include indirect dependents (installed lists always do)")
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: brw uses [--installed] [--recursive] FORMULA")
	}
	var names []string
	if *installed {
		kegs, err := e.kegs(*prefix)
		if err != nil {
			return err
		}
		names = deps.InstalledUses(kegs, pos[0])
	} else {
		idx, err := e.index(context.Background())
		if err != nil {
			return err
		}
		r, err := deps.NewResolver(idx, e.host)
		if err != nil {
			return err
		}
		if names, err = r.Uses(pos[0], *recursive); err != nil {
			return err
		}
	}
	_, err = io.WriteString(e.out, lines(names))
	return err
}

type outdatedJSON struct {
	Name              string   `json:"name"`
	InstalledVersions []string `json:"installed_versions"`
	CurrentVersion    string   `json:"current_version"`
	Pinned            bool     `json:"pinned"`
	PinnedVersion     *string  `json:"pinned_version"`
}

func outdatedCommand(e *env, args []string) error {
	fs := newFlagSet("outdated")
	prefix := prefixFlag(e, fs)
	asJSON := fs.Bool("json", false, "print JSON")
	var quiet, verbose bool
	fs.BoolVar(&quiet, "quiet", false, "names only")
	fs.BoolVar(&quiet, "q", false, "names only")
	fs.BoolVar(&verbose, "verbose", false, "show versions")
	fs.BoolVar(&verbose, "v", false, "show versions")
	var formulaOnly, caskOnly bool
	fs.BoolVar(&formulaOnly, "formula", false, "only formulae")
	fs.BoolVar(&formulaOnly, "formulae", false, "only formulae")
	fs.BoolVar(&caskOnly, "cask", false, "only casks")
	fs.BoolVar(&caskOnly, "casks", false, "only casks")
	var o outdated.CaskOptions
	fs.BoolVar(&o.Greedy, "greedy", false, "include casks that update themselves or are unversioned")
	fs.BoolVar(&o.GreedyLatest, "greedy-latest", false, "include unversioned casks")
	fs.BoolVar(&o.GreedyAutoUpdates, "greedy-auto-updates", false, "include casks that update themselves")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) != 0 {
		return errors.New("usage: brw outdated [--formula|--cask] [--greedy] [--json] [--quiet] [--verbose]")
	}
	if formulaOnly && caskOnly {
		return errors.New("--formula and --cask are mutually exclusive")
	}

	var formulae, casks []outdated.Result
	if !caskOnly {
		kegs, err := e.kegs(*prefix)
		if err != nil {
			return err
		}
		idx, err := e.index(context.Background())
		if err != nil {
			return err
		}
		if formulae, err = outdated.Check(idx, cellar.Prefix(*prefix), kegs); err != nil {
			return err
		}
	}
	if !formulaOnly {
		var err error
		if casks, err = e.outdatedCasks(*prefix, o); err != nil {
			return err
		}
	}

	if *asJSON {
		doc := struct {
			Formulae []outdatedJSON `json:"formulae"`
			Casks    []outdatedJSON `json:"casks"`
		}{Formulae: outdatedEntries(formulae), Casks: outdatedEntries(casks)}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}

	long := !quiet && (verbose || e.tty)
	var b strings.Builder
	for _, r := range formulae {
		b.WriteString(r.Name)
		if long {
			b.WriteString(" (" + strings.Join(r.InstalledVersions, ", ") + ") < " + r.CurrentVersion + pinnedAt(r))
		}
		b.WriteByte('\n')
	}
	for _, r := range casks {
		b.WriteString(r.Name)
		if long {
			b.WriteString(" (" + r.InstalledVersions[0] + ") != " + r.CurrentVersion + pinnedAt(r))
		}
		b.WriteByte('\n')
	}
	_, err := io.WriteString(e.out, b.String())
	return err
}

func pinnedAt(r outdated.Result) string {
	if !r.Pinned {
		return ""
	}
	return " [pinned at " + r.PinnedVersion + "]"
}

func outdatedEntries(results []outdated.Result) []outdatedJSON {
	out := []outdatedJSON{}
	for _, r := range results {
		j := outdatedJSON{r.Name, r.InstalledVersions, r.CurrentVersion, r.Pinned, nil}
		if r.Pinned {
			j.PinnedVersion = &r.PinnedVersion
		}
		out = append(out, j)
	}
	return out
}

// outdatedCasks checks the installed casks against the cask listing, which is
// only fetched when there are casks to check.
func (e *env) outdatedCasks(prefix string, o outdated.CaskOptions) ([]outdated.Result, error) {
	installed, err := caskroom.Installed(prefix)
	if err != nil || len(installed) == 0 {
		return nil, err
	}
	c, err := e.apiClient()
	if err != nil {
		return nil, err
	}
	list, err := c.Casks(context.Background())
	if err != nil {
		return nil, err
	}
	if o.Tag, err = platform.Tag(e.host); err != nil {
		return nil, err
	}
	o.GreedyTokens = strings.Fields(os.Getenv("HOMEBREW_UPGRADE_GREEDY_CASKS"))
	o.NoAutoUpdates = os.Getenv("HOMEBREW_NO_UPGRADE_AUTO_UPDATES_CASKS") != ""
	o.AppDir = func(token string) string { return caskroom.AppDir(prefix, token) }
	o.Bundle = outdated.ReadBundle
	o.Home, _ = os.UserHomeDir()
	return outdated.CheckCasks(api.NewCaskIndex(list), prefix, installed, o)
}

func hasKeg(kegs []cellar.Keg, name string) bool {
	for _, k := range kegs {
		if k.Name == name {
			return true
		}
	}
	return false
}

// lines joins names one per line, with a final newline unless there are none.
func lines(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, "\n") + "\n"
}

func infoCommand(e *env, args []string) error {
	fs := newFlagSet("info")
	prefix := prefixFlag(e, fs)
	asJSON := fs.Bool("json", false, "print JSON")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw info [--json] FORMULA [FORMULA ...]")
	}
	idx, client, err := e.indexAndClient(context.Background())
	if err != nil {
		return err
	}
	kegs, err := e.kegs(*prefix)
	if err != nil {
		return err
	}
	stale, err := outdated.Check(idx, cellar.Prefix(*prefix), kegs)
	if err != nil {
		return err
	}
	env := info.Env{Index: idx, Host: e.host, Prefix: cellar.Prefix(*prefix), Kegs: kegs, Outdated: stale}

	var formulae []*api.Formula
	for _, name := range names {
		f, ok := idx.Lookup(name)
		if !ok {
			return fmt.Errorf("no available formula with the name %q", name)
		}
		formulae = append(formulae, f)
	}

	if *asJSON {
		entries := []json.RawMessage{}
		for _, f := range formulae {
			raw, err := client.Raw(f.Name)
			if err != nil {
				return err
			}
			entry, err := info.JSON(raw, f, env)
			if err != nil {
				return err
			}
			entries = append(entries, entry)
		}
		enc := json.NewEncoder(e.out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Formulae []json.RawMessage `json:"formulae"`
			Casks    []struct{}        `json:"casks"`
		}{entries, []struct{}{}})
	}

	var b strings.Builder
	for i, f := range formulae {
		text, err := info.Text(f, env)
		if err != nil {
			return err
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	_, err = io.WriteString(e.out, b.String())
	return err
}
