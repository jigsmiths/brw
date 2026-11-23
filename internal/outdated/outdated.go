// Package outdated finds installed formulae that are behind the API.
package outdated

import (
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/version"
)

type Result struct {
	Name              string
	FullName          string // tap/name for a tap's formula, as brew prints it
	InstalledVersions []string
	CurrentVersion    string
	Pinned            bool
	PinnedVersion     string
	NoNewerHead       bool // brew prints "latest HEAD" for it
	Head              bool
	JSONVersion       string // brew's JSON current_version: "HEAD" for a HEAD formula unless a fetch found a newer commit
}

type installed struct {
	version string
	pkg     version.PkgVersion
	parsed  bool
	scheme  int
}

// HeadState is what a formula's head repository says about installed.
type HeadState struct {
	Repository bool   // false for a head brw cannot ask, which is never outdated
	Latest     string // empty when unknown
	Newer      bool   // set only when asked to fetch
}

type HeadCommit func(f *api.Formula, installed string, fetch bool) (HeadState, error)

type Options struct {
	Head      HeadCommit
	FetchHead bool
}

// Check returns the outdated formulae, sorted by name. A rack is checked
// against the formula of the tap its kegs came from; one the index has no
// formula for is skipped, and OtherTap names it.
func Check(idx *api.Index, prefix cellar.Prefix, kegs []cellar.Keg) ([]Result, error) {
	return CheckWith(idx, prefix, kegs, Options{})
}

func CheckWith(idx *api.Index, prefix cellar.Prefix, kegs []cellar.Keg, o Options) ([]Result, error) {
	var out []Result
	for name, group := range racks(kegs) {
		f, group, ok := Formula(idx, name, group)
		if !ok {
			continue
		}
		r, outdated, err := evaluate(prefix, f, group, o)
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

// A receipt without a tap counts as a match.
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
// pinned.
func evaluate(prefix cellar.Prefix, f *api.Formula, kegs []cellar.Keg, o Options) (Result, bool, error) {
	latest, err := version.NewPkg(f.Versions.Stable, f.Revision)
	if err != nil {
		return Result{}, false, err
	}
	pinnedVersion, pinned := prefix.Pinned(f.Name)

	all := make([]installed, 0, len(kegs))
	var head *cellar.Keg
	current := false
	for i, k := range kegs {
		if pv, err := version.ParsePkg(k.Version); err == nil && pv.Version.IsHead() && (head == nil || newerHead(k, *head)) {
			head = &kegs[i]
		}
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
	if current {
		return Result{}, false, nil
	}
	var verdict headVerdict
	if head != nil {
		var err error
		if verdict, err = headOutdated(f, *head, o); err != nil || !verdict.outdated {
			return Result{}, false, err
		}
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
	if head != nil && f.Versions.Head != "" && prefix.DefaultKeg(kegs).Receipt.IsHeadBuild() {
		r.Head, r.CurrentVersion = true, head.Version
		if verdict.latest != "" {
			r.CurrentVersion = withRevision("HEAD-"+verdict.latest, f.Revision)
		}
		r.NoNewerHead = slices.Contains(r.InstalledVersions, r.CurrentVersion)
		r.JSONVersion = "HEAD"
		if verdict.fetched {
			r.JSONVersion = r.CurrentVersion
		}
	}
	return r, true, nil
}

func withRevision(v string, revision int) string {
	if revision > 0 {
		return v + "_" + strconv.Itoa(revision)
	}
	return v
}

func newerHead(a, b cellar.Keg) bool {
	ta, tb := modified(a), modified(b)
	if ta != tb {
		return ta > tb
	}
	pa, _ := version.ParsePkg(a.Version)
	pb, _ := version.ParsePkg(b.Version)
	return pa.Revision > pb.Revision
}

func modified(k cellar.Keg) int64 {
	if k.Receipt == nil {
		return 0
	}
	return k.Receipt.SourceModifiedTime
}

// headOutdated is brew's head_version_outdated?: the formula moved to a newer
// version scheme or stable version than the HEAD keg's receipt records, or,
// with FetchHead, its repository has a commit the keg is not at.
func headOutdated(f *api.Formula, k cellar.Keg, o Options) (headVerdict, error) {
	if k.Receipt != nil {
		if k.Receipt.VersionScheme() < f.VersionScheme {
			return headVerdict{outdated: true, latest: cachedCommit(f, o)}, nil
		}
		was, err1 := version.Parse(k.Receipt.Source.StableAtInstall)
		now, err2 := version.Parse(f.Versions.Stable)
		if k.Receipt.Source.StableAtInstall != "" && err1 == nil && err2 == nil && was.Compare(now) < 0 {
			return headVerdict{outdated: true, latest: cachedCommit(f, o)}, nil
		}
	}
	if !o.FetchHead || o.Head == nil || f.Versions.Head == "" {
		return headVerdict{}, nil
	}
	pv, _ := version.ParsePkg(k.Version)
	s, err := o.Head(f, strings.TrimPrefix(pv.Version.String(), "HEAD-"), true)
	if err != nil || !s.Repository {
		return headVerdict{}, err
	}
	return headVerdict{outdated: s.Newer, latest: s.Latest, fetched: true}, nil
}

type headVerdict struct {
	outdated bool
	latest   string
	fetched  bool // latest came from asking the repository, not the cached clone
}

func cachedCommit(f *api.Formula, o Options) string {
	if o.Head == nil || f.Versions.Head == "" {
		return ""
	}
	s, _ := o.Head(f, "", false)
	return s.Latest
}
