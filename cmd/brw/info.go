package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/info"
	"github.com/jigsmiths/brw/internal/outdated"
)

func infoCommand(e *env, args []string) error {
	fs := newFlagSet("info")
	prefix := prefixFlag(e, fs)
	asJSON := fs.Bool("json", false, "print JSON")
	kinds := kindFlags(fs)
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return errors.New("usage: brw info [--formula|--cask] [--json] NAME [NAME ...]")
	}
	formulaOnly, caskOnly, err := kinds()
	if err != nil {
		return err
	}
	ctx := context.Background()
	kegs, err := e.kegs(*prefix)
	if err != nil {
		return err
	}

	// Like brew, a name is a formula if there is one, else a cask.
	var idx *api.Index
	var client *api.Client
	if !caskOnly {
		if idx, client, err = e.indexAndClient(ctx); err != nil {
			return err
		}
	}
	var casks *api.CaskIndex
	type item struct {
		formula *api.Formula
		cask    *api.Cask
	}
	var items []item
	for _, name := range names {
		if idx != nil {
			if f, ok := idx.Lookup(name); ok {
				items = append(items, item{formula: f})
				continue
			}
		}
		if !formulaOnly {
			if casks == nil {
				if casks, err = e.caskIndex(ctx); err != nil {
					return err
				}
			}
			if c, ok := casks.Lookup(name); ok {
				items = append(items, item{cask: c})
				continue
			}
		}
		switch {
		case formulaOnly:
			return fmt.Errorf("no available formula with the name %q", name)
		case caskOnly:
			return fmt.Errorf("no available cask with the name %q", name)
		}
		return fmt.Errorf("no available formula or cask with the name %q", name)
	}

	var formulae []*api.Formula
	for _, it := range items {
		if it.formula != nil {
			formulae = append(formulae, it.formula)
		}
	}
	var env info.Env
	if len(formulae) > 0 {
		stale, err := outdated.Check(idx, cellar.Prefix(*prefix), kegs)
		if err != nil {
			return err
		}
		env = info.Env{Index: idx, Host: e.host, Prefix: cellar.Prefix(*prefix), Kegs: kegs, Outdated: stale}
	}

	if *asJSON {
		if len(formulae) < len(items) {
			return errors.New("info --json for casks is not supported yet")
		}
		entries := []json.RawMessage{}
		for _, f := range formulae {
			raw, err := client.Raw(f.Name)
			if err != nil {
				return err
			}
			entry, err := info.JSON(raw, f, env)
			if err != nil {
				return err
			}
			entries = append(entries, entry)
		}
		enc := json.NewEncoder(e.out)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Formulae []json.RawMessage `json:"formulae"`
			Casks    []struct{}        `json:"casks"`
		}{entries, []struct{}{}})
	}

	var caskEnv info.CaskEnv
	if casks != nil {
		installed, err := caskroom.Installed(*prefix)
		if err != nil {
			return err
		}
		caskEnv = info.CaskEnv{Casks: casks, Host: e.host, Prefix: *prefix, Installed: installed, Kegs: kegs}
		setCaskMachine(&caskEnv.Outdated, *prefix)
		caskEnv.Home = caskEnv.Outdated.Home
		_, statErr := os.Stat(rosettaRuntime)
		caskEnv.Rosetta = statErr == nil
	}

	var b strings.Builder
	for i, it := range items {
		var text string
		if it.formula != nil {
			text, err = info.Text(it.formula, env)
		} else {
			text, err = info.CaskText(it.cask, caskEnv)
		}
		if err != nil {
			return err
		}
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(text)
	}
	_, err = io.WriteString(e.out, b.String())
	return err
}

// rosettaRuntime is there when Rosetta 2 is installed, as brew checks.
const rosettaRuntime = "/Library/Apple/usr/libexec/oah/libRosettaRuntime"
