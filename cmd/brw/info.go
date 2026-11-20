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

	found, err := e.lookupInfoItems(ctx, names, formulaOnly, caskOnly)
	if err != nil {
		return err
	}

	var formulae []*api.Formula
	for _, it := range found.items {
		if it.formula != nil {
			formulae = append(formulae, it.formula)
		}
	}
	var env info.Env
	if len(formulae) > 0 {
		stale, err := outdated.Check(found.idx, cellar.Prefix(*prefix), kegs)
		if err != nil {
			return err
		}
		env = info.Env{Index: found.idx, Host: e.host, Prefix: cellar.Prefix(*prefix), Kegs: kegs, Outdated: stale}
	}

	if *asJSON {
		if len(formulae) < len(found.items) {
			return errors.New("info --json for casks is not supported yet")
		}
		return writeInfoJSON(e.out, found.client, formulae, env)
	}

	var caskEnv info.CaskEnv
	if found.casks != nil {
		if caskEnv, err = e.caskInfoEnv(*prefix, found.casks, kegs); err != nil {
			return err
		}
	}

	var b strings.Builder
	for i, it := range found.items {
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

// infoItem is a name given to info: a formula or a cask.
type infoItem struct {
	formula *api.Formula
	cask    *api.Cask
}

// infoLookup is what info found for its names, and the listings it loaded.
type infoLookup struct {
	items  []infoItem
	idx    *api.Index
	client *api.Client
	casks  *api.CaskIndex // nil unless a name was looked up as a cask
}

// lookupInfoItems finds each name, as brew does: a formula if there is one,
// else a cask. It stops at the first name that is neither.
func (e *env) lookupInfoItems(ctx context.Context, names []string, formulaOnly, caskOnly bool) (infoLookup, error) {
	var l infoLookup
	var err error
	if !caskOnly {
		if l.idx, l.client, err = e.indexAndClient(ctx); err != nil {
			return l, err
		}
	}
	for _, name := range names {
		if l.idx != nil {
			if f, ok := l.idx.Lookup(name); ok {
				l.items = append(l.items, infoItem{formula: f})
				continue
			}
		}
		if !formulaOnly {
			if l.casks == nil {
				if l.casks, err = e.caskIndex(ctx); err != nil {
					return l, err
				}
			}
			if c, ok := l.casks.Lookup(name); ok {
				l.items = append(l.items, infoItem{cask: c})
				continue
			}
		}
		switch {
		case formulaOnly:
			return l, fmt.Errorf("no available formula with the name %q", name)
		case caskOnly:
			return l, fmt.Errorf("no available cask with the name %q", name)
		}
		return l, fmt.Errorf("no available formula or cask with the name %q", name)
	}
	return l, nil
}

// writeInfoJSON is info --json, in brew's shape, for formulae only.
func writeInfoJSON(w io.Writer, client *api.Client, formulae []*api.Formula, env info.Env) error {
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
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	return enc.Encode(struct {
		Formulae []json.RawMessage `json:"formulae"`
		Casks    []struct{}        `json:"casks"`
	}{entries, []struct{}{}})
}

// caskInfoEnv is what info needs to describe casks on this machine.
func (e *env) caskInfoEnv(prefix string, casks *api.CaskIndex, kegs []cellar.Keg) (info.CaskEnv, error) {
	installed, err := caskroom.Installed(prefix)
	if err != nil {
		return info.CaskEnv{}, err
	}
	env := info.CaskEnv{Casks: casks, Host: e.host, Prefix: prefix, Installed: installed, Kegs: kegs}
	setCaskMachine(&env.Outdated, prefix)
	env.Home = env.Outdated.Home
	_, statErr := os.Stat(rosettaRuntime)
	env.Rosetta = statErr == nil
	return env, nil
}

// rosettaRuntime is there when Rosetta 2 is installed, as brew checks.
const rosettaRuntime = "/Library/Apple/usr/libexec/oah/libRosettaRuntime"
