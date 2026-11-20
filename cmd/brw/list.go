package main

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
)

func list(e *env, args []string) error {
	fs := newFlagSet("list")
	prefix := prefixFlag(e, fs)
	versions := fs.Bool("versions", false, "show installed versions")
	pinned := fs.Bool("pinned", false, "list only pinned formulae")
	kinds := kindFlags(fs)
	rest, err := parse(fs, args)
	if err != nil {
		return err
	}
	formulaOnly, caskOnly, err := kinds()
	if err != nil {
		return err
	}
	if *pinned {
		if len(rest) > 0 {
			return errors.New("--pinned with formula names is not supported yet")
		}
		if caskOnly {
			return errors.New("--pinned with --cask is not supported yet")
		}
		out, err := pinnedList(*prefix, *versions)
		if err == nil {
			_, err = io.WriteString(e.out, out)
		}
		return err
	}

	// Like brew, a terminal gets a heading over each kind unless one was asked for.
	headings := e.tty && !formulaOnly && !caskOnly
	var out string
	if !caskOnly {
		s, err := e.listFormulae(*prefix, *versions, headings)
		if err != nil {
			return err
		}
		out += s
	}
	if !formulaOnly {
		s, err := e.listCasks(*prefix, *versions, headings)
		if err != nil {
			return err
		}
		out += s
	}
	_, err = io.WriteString(e.out, out)
	return err
}

// listFormulae is list's formula section: names, or names and versions.
func (e *env) listFormulae(prefix string, versions, heading bool) (string, error) {
	if versions {
		kegs, err := e.kegs(prefix)
		if err != nil {
			return "", err
		}
		return formatVersions(kegs), nil
	}
	names, err := cellar.Prefix(prefix).Names()
	if err != nil {
		return "", err
	}
	if len(names) > 0 && heading {
		return "==> Formulae\n" + lines(names) + "\n", nil
	}
	return lines(names), nil
}

// listCasks is list's cask section, warning about broken Caskroom links.
func (e *env) listCasks(prefix string, versions, heading bool) (string, error) {
	if versions {
		casks, err := caskroom.Installed(prefix)
		if err != nil {
			return "", err
		}
		var b strings.Builder
		for _, c := range casks {
			b.WriteString(c.Token + " " + c.Version + "\n")
		}
		return b.String(), nil
	}
	names, broken, err := caskroom.Names(prefix)
	if err != nil {
		return "", err
	}
	if len(broken) > 0 {
		_, _ = fmt.Fprintf(e.err, "brw: warning: broken Caskroom symlinks: %s\n", strings.Join(broken, ", "))
	}
	if len(names) > 0 && heading {
		return "==> Casks\n" + lines(names), nil
	}
	return lines(names), nil
}

// formatVersions matches the output of brew list --versions.
func formatVersions(kegs []cellar.Keg) string {
	var b strings.Builder
	for i, k := range kegs {
		if i == 0 || kegs[i-1].Name != k.Name {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(k.Name)
		}
		b.WriteString(" " + k.Version)
	}
	if len(kegs) > 0 {
		b.WriteByte('\n')
	}
	return b.String()
}
