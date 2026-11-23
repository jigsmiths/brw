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
	// LatestHead says a HEAD formula is outdated with no newer commit known:
	// brew prints "latest HEAD" for it, and "HEAD" in JSON.
	LatestHead bool
	// Head says the formula is installed from its head spec, so an upgrade
	// builds the head again. JSONVersion is then brew's JSON current_version:
	// "HEAD" unless a fetch found the newer commit.
	Head        bool
	JSONVersion string
}

type installed struct {
	version string
	pkg     version.PkgVersion
	parsed  bool
	scheme  int
}

// HeadCommit is the latest commit of a formula's head repository, and with
// fetch, whether it is newer than installed, the commit of the HEAD keg.
// vcs is false for a head brew cannot ask.
type HeadCommit func(f *api.Formula, installed string, fetch bool) (latest string, newer, vcs bool, err error)

// Options are outdated's --fetch-HEAD: Head finds the commits it compares.
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

// CheckWith is Check, asking head repositories for their commits as o says.
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
// pinned. Else the latest HEAD keg keeps it current unless headOutdated.
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
	var newCommit string
	var fetched bool
	if head != nil {
		outdated, commit, didFetch, err := headOutdated(f, *head, o)
		if err != nil || !outdated {
			return Result{}, false, err
		}
		newCommit, fetched = commit, didFetch
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
	// brew's Formula#latest_head_pkg_version, for a formula loaded from a HEAD keg
	if def := prefix.DefaultKeg(kegs); head != nil && f.Versions.Head != "" && def.Receipt != nil && def.Receipt.Source.Spec == "head" {
		r.Head, r.CurrentVersion = true, head.Version
		if newCommit != "" {
			r.CurrentVersion = withRevision("HEAD-"+newCommit, f.Revision)
		}
		r.LatestHead = slices.Contains(r.InstalledVersions, r.CurrentVersion)
		r.JSONVersion = "HEAD"
		if fetched {
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

// newerHead orders HEAD kegs as brew's latest_head_version does: by the
// receipt's source time, then revision.
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
// with FetchHead, its repository has a commit the keg is not at. commit is
// the repository's latest as fetched, else as the cached clone has it.
func headOutdated(f *api.Formula, k cellar.Keg, o Options) (outdated bool, commit string, fetched bool, err error) {
	if k.Receipt != nil {
		if k.Receipt.VersionScheme() < f.VersionScheme {
			return true, cachedCommit(f, o), false, nil
		}
		was, err1 := version.Parse(k.Receipt.Source.Stable)
		now, err2 := version.Parse(f.Versions.Stable)
		if k.Receipt.Source.Stable != "" && err1 == nil && err2 == nil && was.Compare(now) < 0 {
			return true, cachedCommit(f, o), false, nil
		}
	}
	if !o.FetchHead || o.Head == nil || f.Versions.Head == "" {
		return false, "", false, nil
	}
	pv, _ := version.ParsePkg(k.Version)
	commit, newer, vcs, err := o.Head(f, strings.TrimPrefix(pv.Version.String(), "HEAD-"), true)
	if err != nil || !vcs {
		return false, "", false, err
	}
	return newer, commit, true, nil
}

// cachedCommit is the commit brew's last_commit reads without fetching: from
// GitHub's API or the cached clone, or "" when neither has one.
func cachedCommit(f *api.Formula, o Options) string {
	if o.Head == nil || f.Versions.Head == "" {
		return ""
	}
	commit, _, _, _ := o.Head(f, "", false)
	return commit
}
