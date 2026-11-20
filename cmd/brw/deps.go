package main

import (
	"context"
	"errors"
	"io"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/deps"
)

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

func hasKeg(kegs []cellar.Keg, name string) bool {
	for _, k := range kegs {
		if k.Name == name {
			return true
		}
	}
	return false
}
