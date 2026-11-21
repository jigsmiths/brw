package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/tap"
)

// updateCommand refreshes the formula and cask listings now, rather than when
// the cached ones are ten minutes old, and pulls the prefix's taps.
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

// pullTaps pulls every tap, as brew update does, and names those that moved.
// A tap that cannot be pulled is left with a warning.
func (e *env) pullTaps(ctx context.Context, prefix string) ([]string, error) {
	taps, err := tap.List(prefix)
	if err != nil {
		return nil, err
	}
	var updated []string
	for _, t := range taps {
		changed, err := tap.Pull(ctx, t)
		if err != nil {
			_, _ = fmt.Fprintf(e.err, "brw: warning: could not update %s: %v\n", t.Name, err)
			continue
		}
		if changed {
			updated = append(updated, t.Name)
		}
	}
	return updated, nil
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

// listingSummary is a line for a listing that changed, or "".
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
