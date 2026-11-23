// Package outdated finds installed formulae that are behind the API.
package outdated

import (
	"slices"
	"sort"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/version"
)

// Result is an outdated formula, as brew outdated reports it.
type Result struct {
	Name              string // the rack
	FullName          string // tap/name for a tap's formula, as brew prints it
	InstalledVersions []string
	CurrentVersion    string
	Pinned            bool
	PinnedVersion     string
}

type installed struct {
	version string
	pkg     version.PkgVersion
	parsed  bool
	scheme  int
}

// Check returns the outdated formulae, sorted by name. A rack is checked
// against the formula of the tap its kegs came from; one the index has no
// formula for is skipped, and OtherTap names it.
func Check(idx *api.Index, prefix cellar.Prefix, kegs []cellar.Keg) ([]Result, error) {
	var out []Result
	for name, group := range racks(kegs) {
		f, group, ok := Formula(idx, name, group)
		if !ok {
			continue
		}
		r, outdated, err := evaluate(prefix, f, group)
		if err != nil {
			return nil, err
		}
		if outdated {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FullName < out[j].FullName })
	return out, nil
}

func racks(kegs []cellar.Keg) map[string][]cellar.Keg {
	byName := map[string][]cellar.Keg{}
	for _, k := range kegs {
		byName[k.Name] = append(byName[k.Name], k)
	}
	return byName
}

// KegTap is the tap a rack's kegs came from when it is not homebrew/core and
// they agree on it, else "".
func KegTap(kegs []cellar.Keg) string {
	tap := ""
	for _, k := range kegs {
		if k.Receipt == nil || k.Receipt.Source.Tap == "" || k.Receipt.Source.Tap == "homebrew/core" {
			return ""
		}
		if tap != "" && k.Receipt.Source.Tap != tap {
			return ""
		}
		tap = k.Receipt.Source.Tap
	}
	return tap
}

// Formula is the formula a rack was installed from, as Formulary.from_keg
// finds it: its tap's when its kegs came from one, else the API's under that
// exact name. kegs are those of the rack from that formula's tap.
func Formula(idx *api.Index, name string, group []cellar.Keg) (*api.Formula, []cellar.Keg, bool) {
	tap := KegTap(group)
	f, ok := idx.LookupFrom(tap, name)
	if !ok || f.Name != name {
		return nil, nil, false
	}
	group = FromTap(group, f.Tap)
	return f, group, len(group) > 0
}

// OtherTap returns, sorted, the racks Check skips because the index has no
// formula for the tap their kegs came from.
func OtherTap(idx *api.Index, kegs []cellar.Keg) []string {
	var out []string
	for name, group := range racks(kegs) {
		if _, _, ok := Formula(idx, name, group); !ok && KegTap(group) != "" {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// FromTap keeps the kegs installed from tap. A receipt without a tap counts as a match.
func FromTap(kegs []cellar.Keg, tap string) []cellar.Keg {
	var out []cellar.Keg
	for _, k := range kegs {
		if k.Receipt == nil || k.Receipt.Source.Tap == "" || k.Receipt.Source.Tap == tap {
			out = append(out, k)
		}
	}
	return out
}

// evaluate follows Formula#outdated_kegs: the formula is current if any keg is at
// least the latest version (on the same or a newer scheme) and is linked or
// pinned. A HEAD keg is never outdated.
func evaluate(prefix cellar.Prefix, f *api.Formula, kegs []cellar.Keg) (Result, bool, error) {
	latest, err := version.NewPkg(f.Versions.Stable, f.Revision)
	if err != nil {
		return Result{}, false, err
	}
	pinnedVersion, pinned := prefix.Pinned(f.Name)

	all := make([]installed, 0, len(kegs))
	hasHead, current := false, false
	for _, k := range kegs {
		in := installed{version: k.Version}
		if k.Receipt != nil {
			in.scheme = k.Receipt.VersionScheme()
		}
		in.pkg, err = version.ParsePkg(k.Version)
		in.parsed = err == nil
		all = append(all, in)
		if !in.parsed || current {
			continue
		}
		if in.pkg.Version.IsHead() {
			hasHead = true
			continue
		}
		if f.VersionScheme > in.scheme && latest.Compare(in.pkg) != 0 {
			continue
		}
		if f.VersionScheme == in.scheme && latest.Compare(in.pkg) > 0 {
			continue
		}
		optVersion, _ := prefix.OptVersion(f.Name)
		linked := optVersion == k.Version || prefix.Linked(f.Name, k.Version)
		if linked || pinned {
			current = true
		}
	}
	if current || hasHead {
		return Result{}, false, nil
	}

	slices.SortStableFunc(all, func(a, b installed) int {
		switch {
		case a.parsed != b.parsed:
			if !a.parsed {
				return -1
			}
			return 1
		case a.scheme != b.scheme:
			return a.scheme - b.scheme
		case !a.parsed:
			return 0
		}
		return a.pkg.Compare(b.pkg)
	})
	r := Result{Name: f.Name, FullName: f.FullName, CurrentVersion: latest.String(), Pinned: pinned, PinnedVersion: pinnedVersion}
	if r.FullName == "" {
		r.FullName = f.Name
	}
	for _, in := range all {
		r.InstalledVersions = append(r.InstalledVersions, in.version)
	}
	return r, true, nil
}
