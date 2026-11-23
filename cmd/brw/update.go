package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/tap"
)

func updateCommand(e *env, args []string) error {
	fs := newFlagSet("update")
	prefix := prefixFlag(e, fs)
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("usage: brw update")
	}
	c, err := e.apiClient()
	if err != nil {
		return err
	}
	formulae, err := c.Refresh(context.Background())
	if err != nil {
		return err
	}
	casks, err := c.RefreshCasks(context.Background())
	if err != nil {
		return err
	}
	taps, err := e.pullTaps(context.Background(), *prefix)
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(e.out, updateSummary(formulae, casks, taps))
	return err
}

func (e *env) pullTaps(ctx context.Context, prefix string) ([]string, error) {
	taps, err := tap.List(prefix)
	if err != nil {
		return nil, err
	}
	type result struct {
		changed bool
		err     error
	}
	results := make([]result, len(taps))
	slots := make(chan struct{}, maxPulls)
	var wg sync.WaitGroup
	for i, t := range taps {
		if t.Name == "homebrew/core" {
			continue // read from the API, as brew does unless told not to
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			changed, err := e.pullTap(ctx, t)
			results[i] = result{changed, err}
		})
	}
	wg.Wait()
	var updated []string
	for i, r := range results {
		if r.err != nil {
			_, _ = fmt.Fprintf(e.err, "brw: warning: could not update %s: %v\n", taps[i].Name, r.err)
			continue
		}
		if r.changed {
			updated = append(updated, taps[i].Name)
		}
	}
	return updated, nil
}

const maxPulls = 8

func (e *env) pullTap(ctx context.Context, t tap.Tap) (bool, error) {
	if e.pull != nil {
		return e.pull(ctx, t)
	}
	api := cmp.Or(e.githubAPI, tap.GitHubAPI)
	if tap.Unchanged(ctx, http.DefaultClient, api, t) {
		return false, nil
	}
	return tap.Pull(ctx, t)
}

func updateSummary(formulae, casks api.Update, taps []string) string {
	if !formulae.Changed && !casks.Changed && len(taps) == 0 {
		return "Already up-to-date.\n"
	}
	s := listingSummary("formula", "formulae", formulae) + listingSummary("cask", "casks", casks)
	if len(taps) > 0 {
		s += fmt.Sprintf("Updated %d %s (%s).\n", len(taps), plural(len(taps), "tap", "taps"), sentence(taps))
	}
	return s
}

func listingSummary(kind, plural string, up api.Update) string {
	if !up.Changed {
		return ""
	}
	s := fmt.Sprintf("Updated the %s listing: %d %s", kind, up.Total, plural)
	var parts []string
	if n := len(up.Added); n > 0 {
		parts = append(parts, fmt.Sprintf("+%d new", n))
	}
	if n := len(up.Removed); n > 0 {
		parts = append(parts, fmt.Sprintf("-%d removed", n))
	}
	if len(parts) > 0 {
		s += " (" + strings.Join(parts, ", ") + ")"
	}
	return s + "\n"
}
