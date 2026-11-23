package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/search"
)

func searchCommand(e *env, args []string) error {
	fs := newFlagSet("search")
	prefix := prefixFlag(e, fs)
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
		if idx, err = e.index(ctx, *prefix); err != nil {
			return err
		}
	}
	var casks *api.CaskIndex
	if !formulaOnly {
		if casks, err = e.caskIndex(ctx, *prefix); err != nil {
			return err
		}
	}

	var out string
	if *desc {
		out, err = searchDescriptions(idx, casks, query)
	} else {
		racks, _ := cellar.Prefix(*prefix).Names()
		shown := func(f *api.Formula) bool { return f.ForOS(e.host.OS) || slices.Contains(racks, f.Name) }
		out, err = searchNames(idx, casks, query, shown, e.tty)
	}
	if err != nil {
		return err
	}
	_, err = io.WriteString(e.out, out)
	return err
}

// A nil index is a kind not searched; brew prints a heading for each searched kind even with no match.
func searchDescriptions(idx *api.Index, casks *api.CaskIndex, query string) (string, error) {
	var b strings.Builder
	if idx != nil {
		matches, err := search.Descriptions(idx, query)
		if err != nil {
			return "", err
		}
		b.WriteString("==> Formulae\n")
		for _, m := range matches {
			b.WriteString(m.Name + ": " + m.Desc + "\n")
		}
	}
	if casks != nil {
		matches, err := search.CaskDescriptions(casks, query)
		if err != nil {
			return "", err
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
	return b.String(), nil
}

func searchNames(idx *api.Index, casks *api.CaskIndex, query string, shown func(*api.Formula) bool, headings bool) (string, error) {
	var names, tokens []string
	var err error
	if idx != nil {
		if names, err = search.Names(idx, query, shown); err != nil {
			return "", err
		}
	}
	if casks != nil {
		if tokens, err = search.CaskTokens(casks, query); err != nil {
			return "", err
		}
	}
	if len(names)+len(tokens) == 0 {
		return "", fmt.Errorf("no formulae or casks found for %q", query)
	}
	var b strings.Builder
	if len(names) > 0 {
		if headings {
			b.WriteString("==> Formulae\n")
		}
		b.WriteString(lines(names))
	}
	if len(names) > 0 && len(tokens) > 0 {
		b.WriteByte('\n')
	}
	if len(tokens) > 0 {
		if headings {
			b.WriteString("==> Casks\n")
		}
		b.WriteString(lines(tokens))
	}
	return b.String(), nil
}
