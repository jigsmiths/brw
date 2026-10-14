package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/selfupdate"
)

// updateCommand refreshes the formula listing now, rather than when the cached
// one is ten minutes old, and then updates brw itself to the latest release.
func updateCommand(e *env, args []string) error {
	fs := newFlagSet("update")
	noSelf := fs.Bool("no-self", false, "do not update brw itself")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("usage: brw update [--no-self]")
	}
	c, err := e.apiClient()
	if err != nil {
		return err
	}
	up, err := c.Refresh(context.Background())
	if err != nil {
		return err
	}
	if _, err := fmt.Fprint(e.out, updateSummary(up)); err != nil {
		return err
	}
	if *noSelf || os.Getenv("BRW_NO_SELF_UPDATE") != "" || e.selfUpdate == nil {
		return nil
	}
	res, err := e.selfUpdate(context.Background())
	switch {
	case errors.Is(err, selfupdate.ErrNotRelease):
		_, err = fmt.Fprintln(e.out, "brw was not installed from a release, so it is not updated here")
	case err != nil:
		// The listing is refreshed; a failed self-update does not undo that.
		_, err = fmt.Fprintf(e.err, "brw: warning: could not update brw: %v\n", err)
	case res.Updated:
		_, err = fmt.Fprintf(e.out, "Updated brw %s -> %s\n", res.From, res.To)
	default:
		_, err = fmt.Fprintf(e.out, "brw is up to date (%s)\n", res.To)
	}
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
