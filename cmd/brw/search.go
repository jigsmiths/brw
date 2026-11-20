package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/search"
)

func searchCommand(e *env, args []string) error {
	fs := newFlagSet("search")
	desc := fs.Bool("desc", false, "search descriptions")
	kinds := kindFlags(fs)
	pos, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: brw search [--formula|--cask] [--desc] TEXT|/REGEX/")
	}
	formulaOnly, caskOnly, err := kinds()
	if err != nil {
		return err
	}
	query := pos[0]
	ctx := context.Background()
	var idx *api.Index
	if !caskOnly {
		if idx, err = e.index(ctx); err != nil {
			return err
		}
	}
	var casks *api.CaskIndex
	if !formulaOnly {
		if casks, err = e.caskIndex(ctx); err != nil {
			return err
		}
	}

	var b strings.Builder
	if *desc {
		// Brew heads each kind it searched, even when nothing matched.
		if idx != nil {
			matches, err := search.Descriptions(idx, query)
			if err != nil {
				return err
			}
			b.WriteString("==> Formulae\n")
			for _, m := range matches {
				b.WriteString(m.Name + ": " + m.Desc + "\n")
			}
		}
		if casks != nil {
			matches, err := search.CaskDescriptions(casks, query)
			if err != nil {
				return err
			}
			if idx != nil {
				b.WriteByte('\n')
			}
			b.WriteString("==> Casks\n")
			for _, m := range matches {
				b.WriteString(m.Token + ": ")
				if m.Names != "" {
					b.WriteString("(" + m.Names + ") ")
				}
				b.WriteString(m.Desc + "\n")
			}
		}
		_, err = io.WriteString(e.out, b.String())
		return err
	}

	var names, tokens []string
	if idx != nil {
		if names, err = search.Names(idx, query); err != nil {
			return err
		}
	}
	if casks != nil {
		if tokens, err = search.CaskTokens(casks, query); err != nil {
			return err
		}
	}
	if len(names)+len(tokens) == 0 {
		return fmt.Errorf("no formulae or casks found for %q", query)
	}
	if len(names) > 0 {
		if e.tty {
			b.WriteString("==> Formulae\n")
		}
		b.WriteString(lines(names))
	}
	if len(names) > 0 && len(tokens) > 0 {
		b.WriteByte('\n')
	}
	if len(tokens) > 0 {
		if e.tty {
			b.WriteString("==> Casks\n")
		}
		b.WriteString(lines(tokens))
	}
	_, err = io.WriteString(e.out, b.String())
	return err
}
