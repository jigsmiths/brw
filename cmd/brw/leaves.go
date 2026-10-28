package main

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/leaves"
	"github.com/jigsmiths/brw/internal/uninstall"
)

func leavesCommand(e *env, args []string) error {
	fs := newFlagSet("leaves")
	prefixArg := prefixFlag(e, fs)
	var onRequest, asDependency bool
	fs.BoolVar(&onRequest, "installed-on-request", false, "only leaves that were installed by name")
	fs.BoolVar(&onRequest, "r", false, "only leaves that were installed by name")
	fs.BoolVar(&asDependency, "installed-as-dependency", false, "only leaves that were installed as dependencies")
	fs.BoolVar(&asDependency, "p", false, "only leaves that were installed as dependencies")
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(rest) > 0 || (onRequest && asDependency) {
		return errors.New("usage: brw leaves [--installed-on-request | --installed-as-dependency]")
	}
	filter := leaves.All
	switch {
	case onRequest:
		filter = leaves.OnRequest
	case asDependency:
		filter = leaves.AsDependency
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	kegs, err := e.kegs(prefix)
	if err != nil {
		return err
	}
	casks, err := uninstall.CaskDependencies(prefix)
	if err != nil {
		return fmt.Errorf("cannot tell whether a cask needs these formulae: %w", err)
	}
	for _, name := range leaves.List(cellar.Prefix(prefix), kegs, casks, filter) {
		if _, err := fmt.Fprintln(e.out, name); err != nil {
			return err
		}
	}
	return nil
}
