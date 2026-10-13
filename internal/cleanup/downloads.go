package cleanup

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/version"
)

// abandoned is how old an unfinished download must be before no download can
// still be writing to it.
const abandoned = time.Hour

// bottleFile is a cached bottle or its tab: name--version.tag.bottle[.rebuild].ext
var bottleFile = regexp.MustCompile(`^(.+?)--(.+)\.([a-z0-9_]+)\.bottle(?:\.(\d+))?\.(?:tar\.gz|tab\.json)$`)

// CleanDownloads removes cached bottles that are out of date, for versions that
// are not installed (with Scrub), or older than PruneDays, and downloads that
// were never finished. With names, only the bottles of those formulae are looked at.
func (c *Cleaner) CleanDownloads(names []string) error {
	if c.Downloads == "" {
		return nil
	}
	entries, err := os.ReadDir(c.Downloads)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	racks := c.racks()
	var failed []error
	for _, e := range entries {
		abs := filepath.Join(c.Downloads, e.Name())
		fi, err := e.Info()
		if err != nil {
			continue
		}
		age := c.Now().Sub(fi.ModTime())
		var stale bool
		switch m := bottleFile.FindStringSubmatch(e.Name()); {
		case strings.HasPrefix(e.Name(), ".incomplete-") || strings.HasPrefix(e.Name(), ".tmp-"):
			stale = age > abandoned
		case m != nil && len(names) > 0 && !slices.ContainsFunc(names, func(n string) bool { return strings.EqualFold(n, m[1]) }):
			continue
		case m != nil:
			rebuild, _ := strconv.Atoi(m[4])
			stale = c.staleBottle(m[1], m[2], rebuild, age, racks[m[1]])
		}
		if stale {
			if err := c.cleanPath(abs, func() error { return os.RemoveAll(abs) }); err != nil { //fsx:allow download cache
				failed = append(failed, err)
			}
		}
	}
	return errors.Join(failed...)
}

func (c *Cleaner) staleBottle(name, ver string, rebuild int, age time.Duration, kegs []cellar.Keg) bool {
	if c.PruneDays == 0 || (c.PruneDays > 0 && age > time.Duration(c.PruneDays)*24*time.Hour) {
		return true
	}
	installed := slices.ContainsFunc(kegs, func(k cellar.Keg) bool { return k.Version == ver })
	if c.Scrub && !installed {
		return true
	}
	if c.Current == nil {
		return false
	}
	current, ok := c.Current(name)
	if !ok {
		return false
	}
	if ver == current.PkgVersion {
		return rebuild != current.Rebuild
	}
	if installed && !c.removable(name, ver, kegs, current) {
		return false // the installed version is being kept
	}
	have, err1 := version.ParsePkg(ver)
	want, err2 := version.ParsePkg(current.PkgVersion)
	if err1 != nil || err2 != nil {
		return false
	}
	latestInstalled := slices.ContainsFunc(kegs, func(k cellar.Keg) bool { return k.Version == current.PkgVersion })
	return latestInstalled || want.Compare(have) > 0
}

// removable says whether cleanup would remove the keg of this version: it is old,
// the current version is installed, and nothing keeps it.
func (c *Cleaner) removable(name, ver string, kegs []cellar.Keg, current Formula) bool {
	if !slices.ContainsFunc(kegs, func(k cellar.Keg) bool { return k.Version == current.PkgVersion }) {
		return false
	}
	if !slices.ContainsFunc(oldOnes(kegs, current), func(k cellar.Keg) bool { return k.Version == ver }) {
		return false
	}
	if c.cellar().Linked(name, ver) {
		return false
	}
	if v, pinned := c.cellar().Pinned(name); pinned && v == ver {
		return false
	}
	return len(keepmeRefs(filepath.Join(c.Prefix, "Cellar", name, ver))) == 0
}
