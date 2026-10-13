package cleanup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
)

// standardDirs are the directories of the prefix that are always there, which
// pruning and the .DS_Store sweep work in.
var standardDirs = []string{"Frameworks", "bin", "etc", "include", "lib", "opt", "sbin", "share", "var/homebrew/linked"}

func (c *Cleaner) dirs(withCellar bool) []string {
	var out []string
	for _, d := range standardDirs {
		if fi, err := c.Root.Lstat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	if withCellar {
		out = append(out, "Cellar")
	}
	return out
}

// Leftovers removes staging directories that an interrupted install abandoned
// and the kegs a reinstall left behind.
func (c *Cleaner) Leftovers() error {
	var failed []error
	if entries, err := c.Root.ReadDir(fsx.Staging); err == nil {
		for _, e := range entries {
			fi, err := e.Info()
			if err != nil || c.Now().Sub(fi.ModTime()) <= abandoned {
				continue
			}
			rel := path.Join(fsx.Staging, e.Name())
			if err := c.cleanPath(filepath.Join(c.Prefix, rel), func() error { return c.Root.RemoveStaging(rel) }); err != nil {
				failed = append(failed, err)
			}
		}
		if !c.DryRun {
			_ = c.Root.Remove(fsx.Staging) // only succeeds when empty
		}
	}
	racks, _ := c.Root.ReadDir("Cellar")
	for _, rack := range racks {
		if !rack.IsDir() || strings.HasPrefix(rack.Name(), ".") {
			continue
		}
		kegs, err := c.Root.ReadDir(path.Join("Cellar", rack.Name()))
		if err != nil {
			continue
		}
		for _, k := range kegs {
			if !k.IsDir() || !strings.HasSuffix(k.Name(), ".reinstall") {
				continue
			}
			rel := path.Join("Cellar", rack.Name(), k.Name())
			if err := c.cleanPath(filepath.Join(c.Prefix, rel), func() error { return c.Root.RemoveKeg(rel) }); err != nil {
				failed = append(failed, err)
			}
		}
	}
	return errors.Join(failed...)
}

// LockFiles removes lock files that nobody holds, for the named formulae or all.
// Like brew it does nothing in a dry run.
func (c *Cleaner) LockFiles(names []string) error {
	if c.DryRun {
		return nil
	}
	entries, err := c.Root.ReadDir("var/homebrew/locks")
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		if len(names) > 0 && !slices.ContainsFunc(names, func(n string) bool { return e.Name() == strings.ToLower(n)+".formula.lock" }) {
			continue
		}
		rel := path.Join("var/homebrew/locks", e.Name())
		f, err := c.Root.OpenLock(rel)
		if err != nil {
			continue
		}
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			held, err1 := f.Stat()
			onDisk, err2 := c.Root.Lstat(rel)
			if err1 == nil && err2 == nil && os.SameFile(held, onDisk) {
				_ = c.Root.Remove(rel)
			}
		}
		_ = f.Close()
	}
	return nil
}

// DSStore removes .DS_Store files from the standard directories, or from the
// racks of the named formulae. Like brew it does nothing in a dry run.
func (c *Cleaner) DSStore(names []string) error {
	if c.DryRun {
		return nil
	}
	dirs := c.dirs(true)
	if len(names) > 0 {
		dirs = nil
		for _, n := range names {
			dirs = append(dirs, path.Join("Cellar", strings.ToLower(n)))
		}
	}
	for _, d := range dirs {
		c.sweep(d)
	}
	return nil
}

func (c *Cleaner) sweep(dir string) {
	entries, err := c.Root.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		p := path.Join(dir, e.Name())
		switch {
		case e.IsDir():
			c.sweep(p)
		case e.Name() == ".DS_Store":
			_ = c.Root.Remove(p)
		}
	}
}

// PrunePrefix removes broken symlinks from the standard directories and then the
// directories that are left empty, as brew cleanup --prune-prefix does.
func (c *Cleaner) PrunePrefix() error {
	roots := c.dirs(false)
	var dirs []string
	children := map[string]int{}
	links := 0
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := c.Root.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range entries {
			p := path.Join(dir, e.Name())
			switch {
			case e.Type()&fs.ModeSymlink != 0:
				if _, err := os.Stat(filepath.Join(c.Prefix, p)); err == nil {
					continue
				}
				if c.DryRun {
					c.printf(c.Out, "Would remove (broken link): %s\n", filepath.Join(c.Prefix, p))
					if _, ok := children[dir]; ok {
						children[dir]--
					}
				} else if c.Root.Remove(p) == nil {
					links++
				}
			case e.IsDir():
				dirs = append(dirs, p)
				if c.DryRun {
					if inner, err := c.Root.ReadDir(p); err == nil {
						children[p] = len(inner)
					}
				}
				walk(p)
			}
		}
	}
	for _, r := range roots {
		walk(r)
	}
	removed := 0
	for _, d := range slices.Backward(dirs) {
		if c.DryRun {
			if children[d] == 0 {
				c.printf(c.Out, "Would remove (empty directory): %s\n", filepath.Join(c.Prefix, d))
				if _, ok := children[path.Dir(d)]; ok {
					children[path.Dir(d)]--
				}
			}
			continue
		}
		if c.rmdirIfPossible(d) {
			removed++
		}
	}
	if !c.DryRun && links+removed > 0 {
		summary := fmt.Sprintf("Pruned %d symbolic links", links)
		if removed > 0 {
			summary += fmt.Sprintf(" and %d directories", removed)
		}
		c.printf(c.Out, "%s from %s\n", summary, c.Prefix)
	}
	return nil
}

// rmdirIfPossible removes an empty directory, or one that holds only a .DS_Store.
func (c *Cleaner) rmdirIfPossible(dir string) bool {
	if c.Root.Remove(dir) == nil {
		return true
	}
	entries, err := c.Root.ReadDir(dir)
	if err == nil && len(entries) == 1 && entries[0].Name() == ".DS_Store" {
		if c.Root.Remove(path.Join(dir, ".DS_Store")) == nil {
			return c.Root.Remove(dir) == nil
		}
	}
	return false
}
