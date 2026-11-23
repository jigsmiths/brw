package main

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/tap"
	"github.com/jigsmiths/brw/internal/trust"
	"github.com/jigsmiths/brw/internal/version"
)

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

// index loads the formula listing, using the cache when offline, with the
// formulae the prefix's taps list. An empty prefix reads no taps.
func (e *env) index(ctx context.Context, prefix string) (*api.Index, error) {
	idx, _, err := e.indexAndClient(ctx, prefix)
	return idx, err
}

func (e *env) indexAndClient(ctx context.Context, prefix string) (*api.Index, *api.Client, error) {
	c, err := e.apiClient()
	if err != nil {
		return nil, nil, err
	}
	formulae, err := c.Formulae(ctx)
	if err != nil {
		return nil, nil, err
	}
	idx := api.NewIndex(formulae)
	if prefix != "" {
		if err := e.addTapFormulae(idx, prefix); err != nil {
			return nil, nil, err
		}
	}
	return idx, c, nil
}

// addTapFormulae adds the formulae of the prefix's taps that brew would load:
// those of a trusted tap, or trusted one by one. They are evaluated when
// first looked up.
func (e *env) addTapFormulae(idx *api.Index, prefix string) error {
	taps, err := tap.List(prefix)
	if err != nil || len(taps) == 0 {
		return err
	}
	store, err := trust.Load(trust.Path())
	if err != nil {
		return err
	}
	migrations, err := tap.ReverseMigrations(taps)
	if err != nil {
		return err
	}
	tag, err := platform.Tag(e.host)
	if err != nil {
		return err
	}
	for _, t := range taps {
		if t.Name == "homebrew/core" {
			continue // read from the API, as brew does unless told not to
		}
		files, err := t.FormulaFiles()
		if err != nil {
			e.warnOnce(fmt.Sprintf("brw: warning: %v", err))
			continue
		}
		names := slices.Sorted(maps.Keys(files))
		if len(names) == 0 {
			continue
		}
		trusted := func(string) bool { return true }
		if trust.Required() {
			remote := t.Remote()
			if !store.TapTrusted(t.Name, remote) {
				trusted = func(n string) bool { return store.FormulaTrusted(t.Name, remote, n) }
				if !store.PartlyTrusted(t.Name, remote) {
					idx.SkipTap(t.Name, func(tap string) { e.warnOnce(untrustedTapWarning(tap)) })
				}
			}
		}
		alt, err := t.AltNames()
		if err != nil {
			e.warnOnce(fmt.Sprintf("brw: warning: %v", err))
		}
		idx.AddTap(t.Name, names, alt, func(name string) (api.Formula, json.RawMessage, error) {
			if !trusted(name) {
				full := t.Name + "/" + name
				return api.Formula{}, nil, fmt.Errorf("refusing to load formula %s from untrusted tap %s; run `brw trust --formula %s` or `brw trust %s` to trust it", full, t.Name, full, t.Name)
			}
			return t.Formula(name, tag, migrations)
		})
	}
	return nil
}

// ambiguous is brew's error for a bare name that several taps have and the
// API does not.
func ambiguous(idx *api.Index, name string) error {
	full := idx.TapsWith(name)
	if len(full) < 2 {
		return nil
	}
	return fmt.Errorf("formulae found in multiple taps: %s; use the full name, such as %s", strings.Join(full, ", "), full[0])
}

// unreadable is why a tap formula a name stands for could not be evaluated,
// or nil when there is none.
func unreadable(idx *api.Index, name string) error {
	full := []string{name}
	if !strings.Contains(name, "/") {
		full = idx.TapsWith(name)
	}
	for _, f := range full {
		if err := idx.LoadError(f); err != nil {
			return err
		}
	}
	return nil
}

// installedResolution is brew info's choice for a bare name: the formula the
// newest installed keg came from, when that is another tap's. shadows is the
// tap of the formula it replaces.
func installedResolution(idx *api.Index, kegs []cellar.Keg, f *api.Formula) (*api.Formula, string) {
	var latest *cellar.Keg
	for i, k := range kegs {
		if k.Name == f.Name && (latest == nil || olderVersion(latest.Version, k.Version)) {
			latest = &kegs[i]
		}
	}
	if latest == nil || latest.Receipt == nil || latest.Receipt.Source.Tap == "" || latest.Receipt.Source.Tap == f.Tap {
		return f, ""
	}
	other, ok := idx.LookupFrom(latest.Receipt.Source.Tap, f.Name)
	if !ok {
		return f, ""
	}
	return other, f.Tap
}

func olderVersion(a, b string) bool {
	va, errA := version.ParsePkg(a)
	vb, errB := version.ParsePkg(b)
	if errA != nil || errB != nil {
		return a < b
	}
	return va.Compare(vb) < 0
}

func untrustedTapWarning(name string) string {
	return fmt.Sprintf("brw: warning: Skipping %s because it is not trusted. Run `brw trust %s` to trust it.", name, name)
}

// warnings are printed once per command, though both listings may meet them.
type warnings struct {
	mu   sync.Mutex
	seen map[string]bool
}

func (e *env) warnOnce(msg string) {
	w := e.warned
	if w == nil {
		_, _ = fmt.Fprintln(e.err, msg)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.seen[msg] {
		return
	}
	if w.seen == nil {
		w.seen = map[string]bool{}
	}
	w.seen[msg] = true
	_, _ = fmt.Fprintln(e.err, msg)
}

// caskIndex loads the cask listing, using the cache when offline, with the
// casks the prefix's taps list. A tap that cannot be read is left out with a
// warning. An empty prefix reads no taps.
func (e *env) caskIndex(ctx context.Context, prefix string) (*api.CaskIndex, error) {
	c, err := e.apiClient()
	if err != nil {
		return nil, err
	}
	idx, err := c.CaskIndex(ctx)
	if err != nil || prefix == "" {
		return idx, err
	}
	taps, err := tap.List(prefix)
	if err != nil {
		return nil, err
	}
	store, err := trust.Load(trust.Path())
	if err != nil {
		return nil, err
	}
	for _, t := range taps {
		e.addTapCasks(idx, t, store)
	}
	return idx, nil
}

// addTapCasks adds the casks of a tap that brew would load: those of a
// trusted tap, or trusted one by one. A tap with none trusted is skipped with
// brew's warning.
func (e *env) addTapCasks(idx *api.CaskIndex, t tap.Tap, store *trust.Store) {
	tag, err := platform.Tag(e.host)
	if err != nil {
		_, _ = fmt.Fprintln(e.err, "brw: warning:", err)
		return
	}
	casks, err := t.Casks(tag)
	if err != nil {
		_, _ = fmt.Fprintln(e.err, "brw: warning:", err)
		return
	}
	if len(casks) == 0 || !trust.Required() {
		idx.AddTap(casks)
		return
	}
	remote := t.Remote()
	if store.TapTrusted(t.Name, remote) {
		idx.AddTap(casks)
		return
	}
	if !store.PartlyTrusted(t.Name, remote) {
		e.warnOnce(untrustedTapWarning(t.Name))
		return
	}
	idx.AddTap(slices.DeleteFunc(casks, func(c api.Cask) bool { return !store.CaskTrusted(t.Name, remote, c.Token) }))
}

// tapRemoteURL is where brew taps name from.
func (e *env) tapRemoteURL(name string) string {
	if e.tapRemote != nil {
		return e.tapRemote(name)
	}
	user, repo, _ := strings.Cut(name, "/")
	return "https://github.com/" + user + "/homebrew-" + repo
}
