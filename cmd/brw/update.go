package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
)

// updateCommand refreshes the formula and cask listings now, rather than when
// the cached ones are ten minutes old.
func updateCommand(e *env, args []string) error {
	fs := newFlagSet("update")
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
	_, err = fmt.Fprint(e.out, updateSummary(formulae, casks))
	return err
}

func updateSummary(formulae, casks api.Update) string {
	if !formulae.Changed && !casks.Changed {
		return "Already up-to-date.\n"
	}
	return listingSummary("formula", "formulae", formulae) + listingSummary("cask", "casks", casks)
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
