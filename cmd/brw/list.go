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
	var b strings.Builder
	if !caskOnly {
		if *versions {
			kegs, err := e.kegs(*prefix)
			if err != nil {
				return err
			}
			b.WriteString(formatVersions(kegs))
		} else {
			names, err := cellar.Prefix(*prefix).Names()
			if err != nil {
				return err
			}
			if len(names) > 0 && headings {
				b.WriteString("==> Formulae\n" + lines(names) + "\n")
			} else {
				b.WriteString(lines(names))
			}
		}
	}
	if !formulaOnly {
		if *versions {
			casks, err := caskroom.Installed(*prefix)
			if err != nil {
				return err
			}
			for _, c := range casks {
				b.WriteString(c.Token + " " + c.Version + "\n")
			}
		} else {
			names, broken, err := caskroom.Names(*prefix)
			if err != nil {
				return err
			}
			if len(names) > 0 && headings {
				b.WriteString("==> Casks\n")
			}
			b.WriteString(lines(names))
			if len(broken) > 0 {
				_, _ = fmt.Fprintf(e.err, "brw: warning: broken Caskroom symlinks: %s\n", strings.Join(broken, ", "))
			}
		}
	}
	_, err = io.WriteString(e.out, b.String())
	return err
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
