package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/install"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/tap"
)

type outdatedJSON struct {
	Name              string   `json:"name"`
	InstalledVersions []string `json:"installed_versions"`
	CurrentVersion    string   `json:"current_version"`
	Pinned            bool     `json:"pinned"`
	PinnedVersion     *string  `json:"pinned_version"`
}

func outdatedCommand(e *env, args []string) error {
	fs := newFlagSet("outdated")
	prefix := prefixFlag(e, fs)
	asJSON := fs.Bool("json", false, "print JSON")
	var quiet, verbose bool
	fs.BoolVar(&quiet, "quiet", false, "names only")
	fs.BoolVar(&quiet, "q", false, "names only")
	fs.BoolVar(&verbose, "verbose", false, "show versions")
	fs.BoolVar(&verbose, "v", false, "show versions")
	fetchHead := fs.Bool("fetch-HEAD", false, "fetch each HEAD formula's repository to see whether it is outdated")
	kinds := kindFlags(fs)
	var o outdated.CaskOptions
	fs.BoolVar(&o.Greedy, "greedy", false, "include casks that update themselves or are unversioned")
	fs.BoolVar(&o.GreedyLatest, "greedy-latest", false, "include unversioned casks")
	fs.BoolVar(&o.GreedyAutoUpdates, "greedy-auto-updates", false, "include casks that update themselves")
	if pos, err := parse(fs, args); err != nil {
		return err
	} else if len(pos) != 0 {
		return errors.New("usage: brw outdated [--formula|--cask] [--fetch-HEAD] [--greedy] [--json] [--quiet] [--verbose]")
	}
	formulaOnly, caskOnly, err := kinds()
	if err != nil {
		return err
	}

	shared := *e
	shared.err = &lockedWriter{w: e.err}
	e = &shared
	var casks []outdated.Result
	var caskErr error
	var wg sync.WaitGroup
	if !formulaOnly {
		wg.Go(func() { casks, caskErr = e.outdatedCasks(*prefix, o) })
	}
	var formulae []outdated.Result
	if !caskOnly {
		formulae, err = e.outdatedFormulae(*prefix, *fetchHead)
	}
	wg.Wait()
	if err != nil {
		return err
	}
	if caskErr != nil {
		return caskErr
	}

	if *asJSON {
		doc := struct {
			Formulae []outdatedJSON `json:"formulae"`
			Casks    []outdatedJSON `json:"casks"`
		}{Formulae: outdatedEntries(formulae), Casks: outdatedEntries(casks)}
		b, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(e.out, "%s\n", b)
		return err
	}

	long := !quiet && (verbose || e.tty)
	var b strings.Builder
	for _, r := range formulae {
		b.WriteString(r.FullName)
		if long {
			current := r.CurrentVersion
			if r.NoNewerHead {
				current = "latest HEAD"
			}
			b.WriteString(" (" + strings.Join(r.InstalledVersions, ", ") + ") < " + current + pinnedAt(r))
		}
		b.WriteByte('\n')
	}
	for _, r := range casks {
		b.WriteString(r.Name)
		if long {
			b.WriteString(" (" + r.InstalledVersions[0] + ") != " + r.CurrentVersion + pinnedAt(r))
		}
		b.WriteByte('\n')
	}
	_, err = io.WriteString(e.out, b.String())
	return err
}

func pinnedAt(r outdated.Result) string {
	if !r.Pinned {
		return ""
	}
	return " [pinned at " + r.PinnedVersion + "]"
}

func outdatedEntries(results []outdated.Result) []outdatedJSON {
	out := []outdatedJSON{}
	for _, r := range results {
		name := r.FullName
		if name == "" {
			name = r.Name
		}
		j := outdatedJSON{name, r.InstalledVersions, cmp.Or(r.JSONVersion, r.CurrentVersion), r.Pinned, nil}
		if r.Pinned {
			j.PinnedVersion = &r.PinnedVersion
		}
		out = append(out, j)
	}
	return out
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (e *env) outdatedFormulae(prefix string, fetchHead bool) ([]outdated.Result, error) {
	kegs, err := e.kegs(prefix)
	if err != nil {
		return nil, err
	}
	idx, client, err := e.indexAndClient(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	e.warnOtherTap(idx, kegs, outdated.OtherTap(idx, kegs))
	return outdated.CheckWith(idx, cellar.Prefix(prefix), kegs, e.headOptions(client, fetchHead))
}

func (e *env) headOptions(client *api.Client, fetchHead bool) outdated.Options {
	heads := install.Heads{Cache: client.CacheDir, GitHubAPI: cmp.Or(e.githubAPI, tap.GitHubAPI)}
	return outdated.Options{FetchHead: fetchHead, Head: func(f *api.Formula, installed string, fetch bool) (outdated.HeadState, error) {
		return heads.Check(context.Background(), f, installed, fetch)
	}}
}

func (e *env) warnOtherTap(idx *api.Index, kegs []cellar.Keg, names []string) {
	var missing []string
	for _, name := range names {
		if err := tapFormulaError(idx, kegs, name); err != nil {
			_, _ = fmt.Fprintf(e.err, "brw: warning: not checking %s: %v\n", name, err)
			continue
		}
		missing = append(missing, name)
	}
	if len(missing) > 0 {
		_, _ = fmt.Fprintf(e.err, "brw: warning: not checking formulae from other taps: %s\n", strings.Join(missing, ", "))
	}
}

func tapFormulaError(idx *api.Index, kegs []cellar.Keg, rack string) error {
	var group []cellar.Keg
	for _, k := range kegs {
		if k.Name == rack {
			group = append(group, k)
		}
	}
	return idx.LoadError(outdated.KegTap(group) + "/" + rack)
}

func (e *env) outdatedCasks(prefix string, o outdated.CaskOptions) ([]outdated.Result, error) {
	installed, err := caskroom.Installed(prefix)
	if err != nil || len(installed) == 0 {
		return nil, err
	}
	idx, err := e.caskIndex(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	if o.Tag, err = platform.Tag(e.host); err != nil {
		return nil, err
	}
	setCaskMachine(&o, prefix)
	return outdated.CheckCasks(idx, prefix, installed, o)
}

func setCaskMachine(o *outdated.CaskOptions, prefix string) {
	o.GreedyTokens = strings.Fields(os.Getenv("HOMEBREW_UPGRADE_GREEDY_CASKS"))
	o.NoAutoUpdates = os.Getenv("HOMEBREW_NO_UPGRADE_AUTO_UPDATES_CASKS") != ""
	o.AppDir = func(token string) string { return caskroom.AppDir(prefix, token) }
	o.Bundle = outdated.ReadBundle
	o.Home, _ = os.UserHomeDir()
}
