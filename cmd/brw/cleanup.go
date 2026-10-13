package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/cleanup"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/version"
)

// defaultPruneDays is how long a cached download is kept, as in brew.
const defaultPruneDays = 120

func cleanupCommand(e *env, args []string) (err error) {
	fs := newFlagSet("cleanup")
	prefixArg := prefixFlag(e, fs)
	journal := fs.String("journal", "", "append changes to this file")
	prune := fs.String("prune", "", "remove cached downloads older than this many days, or `all`")
	var dryRun, scrub, prunePrefix bool
	fs.BoolVar(&dryRun, "dry-run", false, "show what would be removed")
	fs.BoolVar(&dryRun, "n", false, "show what would be removed")
	fs.BoolVar(&scrub, "scrub", false, "also remove downloads of what is not installed")
	fs.BoolVar(&scrub, "s", false, "also remove downloads of what is not installed")
	fs.BoolVar(&prunePrefix, "prune-prefix", false, "only remove broken symlinks and empty directories from the prefix")
	names, err := parse(fs, args)
	if err != nil {
		return err
	}
	days := defaultPruneDays
	switch {
	case *prune == "all":
		days = 0
	case *prune != "":
		n, convErr := strconv.Atoi(*prune)
		if convErr != nil || n < 0 {
			return errors.New("`--prune` expects an integer or `all`")
		}
		days = n
	}
	prefix, err := filepath.Abs(*prefixArg)
	if err != nil {
		return err
	}
	kegs, err := cellar.Prefix(prefix).Kegs()
	if err != nil {
		return err
	}
	client, err := e.apiClient()
	if err != nil {
		return err
	}
	return withRoot(prefix, *journal, func(root *fsx.Root) error {
		c := &cleanup.Cleaner{
			Root: root, Prefix: prefix, Linker: &link.Linker{Root: root, Prefix: prefix}, Kegs: kegs,
			Downloads: filepath.Join(client.CacheDir, "downloads"), PruneDays: days, Scrub: scrub,
			Skip: skipCleanup(), Now: time.Now, Out: e.out, Warn: e.err, DryRun: dryRun,
		}
		if prunePrefix {
			return c.PrunePrefix()
		}
		index := sync.OnceValues(func() (*api.Index, error) { return e.index(context.Background()) })
		if idx, err := index(); err == nil {
			c.Current = func(name string) (cleanup.Formula, bool) {
				f, ok := idx.Lookup(name)
				if !ok || f.Name != name {
					return cleanup.Formula{}, false
				}
				v, err := version.NewPkg(f.Versions.Stable, f.Revision)
				if err != nil {
					return cleanup.Formula{}, false
				}
				return cleanup.Formula{PkgVersion: v.String(), Scheme: f.VersionScheme, Rebuild: f.Bottle.Stable.Rebuild}, true
			}
		}
		var failed []error
		failed = append(failed, c.OldKegs(names))
		if fresh, err := cellar.Prefix(prefix).Kegs(); err == nil { // old kegs are gone now
			c.Kegs, kegs = fresh, fresh
		}
		if len(names) == 0 {
			if os.Getenv("HOMEBREW_NO_AUTOREMOVE") == "" {
				failed = append(failed, newRemover(e, root, prefix, kegs, dryRun).Autoremove())
			}
		}
		failed = append(failed, c.CleanDownloads(names))
		if len(names) == 0 {
			failed = append(failed, c.Leftovers())
		}
		failed = append(failed, c.LockFiles(names))
		if len(names) == 0 {
			failed = append(failed, c.PrunePrefix())
		}
		failed = append(failed, c.DSStore(names))
		if c.Freed() > 0 {
			verb := "has freed"
			if dryRun {
				verb = "would free"
			}
			_, _ = fmt.Fprintf(e.out, "==> This operation %s approximately %s of disk space.\n", verb, cellar.ReadableSize(c.Freed()))
		}
		return errors.Join(failed...)
	})
}

// skipCleanup is the set of formulae HOMEBREW_NO_CLEANUP_FORMULAE says to leave alone.
func skipCleanup() func(string) bool {
	list := strings.FieldsFunc(os.Getenv("HOMEBREW_NO_CLEANUP_FORMULAE"), func(r rune) bool { return r == ',' || r == ' ' })
	return func(name string) bool { return slices.Contains(list, name) }
}
