package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jigsmiths/brw/internal/api"
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
