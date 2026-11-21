package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/tap"
	"github.com/jigsmiths/brw/internal/trust"
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
	casks, err := t.Casks()
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
	if !store.HasCasksFrom(t.Name, remote) {
		_, _ = fmt.Fprintf(e.err, "brw: warning: Skipping %s because it is not trusted. Run `brw trust %s` to trust it.\n", t.Name, t.Name)
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
