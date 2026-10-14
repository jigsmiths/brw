package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
)

// updateCommand refreshes the formula listing now, rather than when the cached
// one is ten minutes old. It does not update brw itself.
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
	up, err := c.Refresh(context.Background())
	if err != nil {
		return err
	}
	_, err = fmt.Fprint(e.out, updateSummary(up))
	return err
}

func updateSummary(up api.Update) string {
	if !up.Changed {
		return "Already up-to-date.\n"
	}
	s := fmt.Sprintf("Updated the formula listing: %d formulae", up.Total)
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
