// Package cleanup removes what an install no longer needs, the way brew cleanup does.
package cleanup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/version"
)

// Formula is the current version of a formula, from the listing.
type Formula struct {
	PkgVersion string // the version and revision, such as "1.2_1"
	Scheme     int
	Rebuild    int // of the bottle
}

type Cleaner struct {
	Root   *fsx.Root
	Prefix string // absolute path of Root
	Linker *link.Linker
	Kegs   []cellar.Keg // every installed keg

	// Current says what version of a formula is current. It may be nil when
	// there is no formula listing.
	Current func(name string) (Formula, bool)
	// Skip says which formulae are left alone.
	Skip func(name string) bool
	// Downloads is the directory where bottles are cached, or "".
	Downloads string
	// PruneDays removes cached downloads older than this many days; 0 removes
	// them all and a negative number keeps them whatever their age.
	PruneDays int
	Scrub     bool

	Now       func() time.Time
	Out, Warn io.Writer
	DryRun    bool
	// LocksHeld says the caller already holds the formula locks, as an install
	// does when it cleans up after itself.
	LocksHeld bool

	freed int64
	seen  map[string]bool
}

// Freed is the disk space released, or that would be in a dry run.
func (c *Cleaner) Freed() int64 { return c.freed }

func (c *Cleaner) printf(w io.Writer, format string, args ...any) {
	if w != nil {
		_, _ = fmt.Fprintf(w, format, args...)
	}
}

func (c *Cleaner) warn(format string, args ...any) {
	c.printf(c.Warn, "brw: warning: "+format+"\n", args...)
}

func (c *Cleaner) cellar() cellar.Prefix { return cellar.Prefix(c.Prefix) }

func (c *Cleaner) racks() map[string][]cellar.Keg {
	out := map[string][]cellar.Keg{}
	for _, k := range c.Kegs {
		out[k.Name] = append(out[k.Name], k)
	}
	return out
}

// cleanPath says what is being removed and how much it frees, and removes it
// unless this is a dry run.
func (c *Cleaner) cleanPath(abs string, remove func() error) error {
	if c.seen == nil {
		c.seen = map[string]bool{}
	}
	if c.seen[abs] {
		return nil
	}
	if _, err := os.Lstat(abs); err != nil {
		return nil
	}
	c.seen[abs] = true
	_, size, _ := cellar.Keg{Path: abs}.Usage()
	abv, _ := cellar.Keg{Path: abs}.Abv()
	c.freed += size
	if c.DryRun {
		c.printf(c.Out, "Would remove: %s (%s)\n", abs, abv)
		return nil
	}
	c.printf(c.Out, "Removing: %s... (%s)\n", abs, abv)
	return remove()
}

// OldKegs removes the kegs of formulae that are older than the current version,
// for the named formulae or, with none named, for all.
func (c *Cleaner) OldKegs(names []string) error {
	racks := c.racks()
	var todo []string
	if len(names) == 0 {
		todo = slices.Sorted(func(yield func(string) bool) {
			for n := range racks {
				if !yield(n) {
					return
				}
			}
		})
	}
	for _, n := range names {
		n = strings.TrimPrefix(strings.ToLower(n), "homebrew/core/")
		if c.Skip != nil && c.Skip(n) {
			c.printf(c.Warn, "brw: Refusing to clean %s because it is listed in HOMEBREW_NO_CLEANUP_FORMULAE\n", n)
			continue
		}
		if _, ok := racks[n]; ok {
			todo = append(todo, n)
		}
	}
	if c.Current == nil {
		if len(todo) > 0 {
			c.warn("cannot tell which versions are old without the formula listing")
		}
		return nil
	}
	var failed []error
	for _, name := range todo {
		if len(names) == 0 && c.Skip != nil && c.Skip(name) {
			continue
		}
		if err := c.cleanFormula(name, racks[name]); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

func (c *Cleaner) cleanFormula(name string, kegs []cellar.Keg) (err error) {
	current, ok := c.Current(name)
	if !ok {
		return nil
	}
	latestInstalled := slices.ContainsFunc(kegs, func(k cellar.Keg) bool { return k.Version == current.PkgVersion })
	if !latestInstalled {
		if _, pinned := c.cellar().Pinned(name); !pinned {
			c.warn("Skipping %s: most recent version %s not installed", name, current.PkgVersion)
		}
		return nil
	}
	old := oldOnes(kegs, current)
	if len(old) == 0 {
		return nil
	}
	if !c.DryRun && !c.LocksHeld {
		l, lerr := lock.Acquire(c.Root, name)
		if lerr != nil {
			return lerr
		}
		defer func() { err = errors.Join(err, l.Release()) }()
	}
	var failed []error
	for _, k := range old {
		abs := filepath.Join(c.Prefix, "Cellar", k.Name, k.Version)
		if c.cellar().Linked(k.Name, k.Version) {
			c.warn("Skipping (old) %s due to it being linked", abs)
			continue
		}
		if v, pinned := c.cellar().Pinned(name); pinned && v == k.Version {
			c.warn("Skipping (old) %s due to it being pinned", abs)
			continue
		}
		if refs := keepmeRefs(abs); len(refs) > 0 {
			c.warn("Skipping %s as it is needed by %s", abs, strings.Join(refs, ", "))
			continue
		}
		lk := link.Keg{Name: k.Name, Version: k.Version}
		if err := c.cleanPath(abs, func() error {
			if err := c.Root.RemoveKeg(path.Join("Cellar", k.Name, k.Version)); err != nil {
				return err
			}
			return c.Linker.Forget(lk)
		}); err != nil {
			failed = append(failed, err)
		}
	}
	return errors.Join(failed...)
}

// oldOnes picks the kegs that are older than the current version: from an older
// version scheme, or from the same scheme and an older version.
func oldOnes(kegs []cellar.Keg, current Formula) []cellar.Keg {
	want, err := version.ParsePkg(current.PkgVersion)
	if err != nil {
		return nil
	}
	var old []cellar.Keg
	for _, k := range kegs {
		have, err := version.ParsePkg(k.Version)
		if err != nil {
			continue
		}
		scheme := 0
		if k.Receipt != nil {
			scheme = k.Receipt.VersionScheme()
		}
		if scheme < current.Scheme || (scheme == current.Scheme && want.Compare(have) > 0) {
			old = append(old, k)
		}
	}
	return old
}

// keepmeRefs lists the paths in a keg's .keepme file that exist.
func keepmeRefs(keg string) []string {
	b, err := os.ReadFile(filepath.Join(keg, ".keepme"))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		ref := strings.TrimSpace(line)
		if ref == "" {
			continue
		}
		if _, err := os.Stat(ref); err == nil {
			out = append(out, ref)
		}
	}
	return out
}
