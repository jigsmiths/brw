// Package install plans and carries out installs of bottles into a prefix.
package install

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/deps"
	"github.com/jigsmiths/brw/internal/info"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/poststep"
)

var (
	ErrNoBottle        = errors.New("no bottle for this host")
	ErrDisabled        = errors.New("disabled")
	ErrPostInstall     = errors.New("has post_install steps brw cannot run yet")
	ErrOtherPrefix     = errors.New("its bottle is built for another prefix")
	ErrUnsupportedHost = errors.New("cannot be installed on this host yet")
	ErrConflict        = errors.New("conflicts with a linked formula")
	ErrOtherTap        = errors.New("installed from another tap")
)

// BottleSource finds the file of a bottle for a formula name and tag.
type BottleSource interface {
	Bottles(want map[string]string) (map[string]api.BottleFile, error)
}

type Item struct {
	Formula *api.Formula
	Tag     string
	HostTag string // the tag of this host, which can differ from Tag, as for an "all" bottle
	Arch    string // the CPU of this host as brew names it
	// RuntimeDeps is what the receipt lists as the runtime dependencies, worked out
	// from the formula as brew does. Nil leaves the bottle's list as it is.
	RuntimeDeps []cellar.Dependency
	Bottle      api.BottleFile
	PkgVersion  string // version and revision, which names the keg directory
	OnRequest   bool   // asked for by name, not only needed by something else
	KegOnly     bool
	Warnings    []string

	// Upgrade holds the kegs of the older versions this one replaces, and Linked
	// whether to link the new keg, which an upgrade takes from the old one. Both
	// are unset for a fresh install.
	Upgrade []cellar.Keg
	Linked  *bool
	// Reinstall says the old keg is put aside and the formula installed afresh,
	// and the old keg is deleted once that worked.
	Reinstall bool
}

// Plan lists what to install, dependencies first. Locks names every formula to
// lock while installing: the items and their runtime dependencies.
type Plan struct {
	Items            []Item
	Locks            []string
	AlreadyInstalled []string
}

// Planner works out an install without changing anything.
type Planner struct {
	Index   *api.Index
	Host    platform.Host
	Prefix  string
	Bottles BottleSource
	Kegs    []cellar.Keg
	Linked  func(name string) bool
	Now     time.Time // for deprecation notices; the zero value means now

	upgrading map[string]bool // formulae being upgraded, which count as not installed
}

// Plan resolves the names, adds the dependencies that are not installed yet and
// checks that each formula can be installed from a bottle. Every problem found
// is reported.
func (p *Planner) Plan(names []string) (*Plan, error) {
	r, err := deps.NewResolver(p.Index, p.Host)
	if err != nil {
		return nil, err
	}
	hostTag, err := platform.Tag(p.Host)
	if err != nil {
		return nil, err
	}

	plan := &Plan{}
	var problems []error
	position := map[string]int{} // formula name to index in plan.Items
	skipped := map[string]bool{} // installed formulae met on the way

	var visit func(f *api.Formula, requested bool) error
	visit = func(f *api.Formula, requested bool) error {
		if _, ok := position[f.Name]; ok {
			return nil
		}
		if installed, err := p.installed(f); err != nil {
			problems = append(problems, err)
			return nil
		} else if installed {
			if requested && !skipped[f.Name] {
				plan.AlreadyInstalled = append(plan.AlreadyInstalled, f.Name)
			}
			skipped[f.Name] = true
			return nil
		}
		position[f.Name] = -1 // guards against a dependency cycle
		direct, err := r.Direct(f.Name)
		if err != nil {
			return err
		}
		for _, d := range direct {
			df, ok := p.Index.Lookup(d)
			if !ok {
				return fmt.Errorf("%s: dependency %s: %w", f.Name, d, deps.ErrNotFound)
			}
			if err := visit(df, false); err != nil {
				return err
			}
		}
		position[f.Name] = len(plan.Items)
		g, err := f.ForTag(hostTag)
		if err != nil {
			return err
		}
		runtime, err := p.runtimeDeps(r, f)
		if err != nil {
			return err
		}
		plan.Items = append(plan.Items, Item{Formula: f, HostTag: hostTag, Arch: hostArch(p.Host), RuntimeDeps: runtime, OnRequest: requested, KegOnly: g.KegOnly && !p.autoLinks(f, requested), PkgVersion: pkgVersion(f)})
		return nil
	}

	for _, name := range names {
		f, ok := p.Index.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("%s: %w", name, deps.ErrNotFound)
		}
		if err := visit(f, true); err != nil {
			return nil, err
		}
	}

	want := map[string]string{}
	for i := range plan.Items {
		it := &plan.Items[i]
		if err := p.check(it, &problems); err != nil {
			return nil, err
		}
		if it.Tag != "" {
			want[it.Formula.Name] = it.Tag
		}
	}
	files, err := p.bottles(want)
	if err != nil {
		return nil, err
	}
	for i := range plan.Items {
		it := &plan.Items[i]
		if it.Tag == "" {
			continue
		}
		it.Bottle = files[it.Formula.Name]
		if err := p.checkCellar(it); err != nil {
			problems = append(problems, err)
		}
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	if plan.Locks, err = lockNames(r, plan.Items); err != nil {
		return nil, err
	}
	return plan, nil
}

// lockNames lists the items and everything they need at run time, sorted. The
// dependencies already installed are locked too, as they must not change while
// the items go in on top of them.
func lockNames(r *deps.Resolver, items []Item) ([]string, error) {
	var names []string
	for _, it := range items {
		all, err := r.Recursive(it.Formula.Name)
		if err != nil {
			return nil, err
		}
		names = append(names, it.Formula.Name)
		names = append(names, all...)
	}
	slices.Sort(names)
	return slices.Compact(names), nil
}

// PlanUpgrade plans bringing the named installed formulae up to date. Each keeps
// whether it was installed on request and whether it was linked, keg-only
// formulae come first, and dependencies the new versions need are installed. A
// formula that cannot be upgraded is reported and does not stop the others.
func (p *Planner) PlanUpgrade(names []string) (*Plan, []error) {
	return p.planInstalled(names, false)
}

// PlanReinstall plans installing the named formulae afresh at their current
// version, keeping whether each was on request and linked. A name that is not
// installed is a plain install. Failures are reported one formula at a time.
func (p *Planner) PlanReinstall(names []string) (*Plan, []error) {
	return p.planInstalled(names, true)
}

// planInstalled plans the named formulae as replacements of what is installed.
func (p *Planner) planInstalled(names []string, reinstall bool) (*Plan, []error) {
	hostTag, err := platform.Tag(p.Host)
	if err != nil {
		return &Plan{}, []error{err}
	}
	ordered := p.kegOnlyFirst(names, hostTag)
	p.upgrading = map[string]bool{}
	for _, n := range ordered {
		if f, ok := p.Index.Lookup(n); ok {
			p.upgrading[f.Name] = true
		}
	}
	defer func() { p.upgrading = nil }()

	if plan, err := p.Plan(ordered); err == nil {
		p.markUpgrades(plan, reinstall)
		return plan, nil
	}
	merged, errs := p.PlanEach(ordered)
	p.markUpgrades(merged, reinstall)
	return merged, errs
}

// PlanEach plans each name on its own and merges what worked. A name that cannot
// be installed is reported and does not stop the others.
func (p *Planner) PlanEach(names []string) (*Plan, []error) {
	merged := &Plan{}
	var errs []error
	for _, n := range names {
		one, err := p.Plan([]string{n})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, it := range one.Items {
			i := slices.IndexFunc(merged.Items, func(x Item) bool { return x.Formula.Name == it.Formula.Name })
			if i < 0 {
				merged.Items = append(merged.Items, it)
			} else if it.OnRequest {
				merged.Items[i].OnRequest = true
			}
		}
		merged.Locks = append(merged.Locks, one.Locks...)
		merged.AlreadyInstalled = append(merged.AlreadyInstalled, one.AlreadyInstalled...)
	}
	slices.Sort(merged.Locks)
	merged.Locks = slices.Compact(merged.Locks)
	merged.AlreadyInstalled = slices.Compact(merged.AlreadyInstalled)
	return merged, errs
}

func (p *Planner) kegOnlyFirst(names []string, tag string) []string {
	var first, rest []string
	for _, n := range names {
		f, ok := p.Index.Lookup(n)
		if ok {
			if g, err := f.ForTag(tag); err == nil && g.KegOnly {
				first = append(first, n)
				continue
			}
		}
		rest = append(rest, n)
	}
	return append(first, rest...)
}

// markUpgrades fills in what the items being upgraded take from their old kegs.
func (p *Planner) markUpgrades(plan *Plan, reinstall bool) {
	for i := range plan.Items {
		it := &plan.Items[i]
		if !p.upgrading[it.Formula.Name] {
			continue
		}
		var old []cellar.Keg
		for _, k := range p.Kegs {
			if k.Name == it.Formula.Name {
				old = append(old, k)
			}
		}
		if len(old) == 0 {
			continue
		}
		it.Upgrade = old
		it.Reinstall = reinstall
		current := cellar.Prefix(p.Prefix).DefaultKeg(old)
		it.OnRequest = current.Receipt != nil && current.Receipt.InstalledOnRequest
		// Brew links what it installs unless the formula is keg-only, so an
		// unlinked keg is linked again by the upgrade.
		linked := p.Linked(it.Formula.Name) || !it.KegOnly
		it.Linked = &linked
	}
}

func (p *Planner) bottles(want map[string]string) (map[string]api.BottleFile, error) {
	if len(want) == 0 {
		return nil, nil
	}
	return p.Bottles.Bottles(want)
}

// runtimeDeps lists the runtime dependencies of f for its receipt, as brew
// writes them: in its order, with the versions of the formulae as they are now.
func (p *Planner) runtimeDeps(r *deps.Resolver, f *api.Formula) ([]cellar.Dependency, error) {
	order, err := r.Expanded(f.Name)
	if err != nil {
		return nil, err
	}
	direct, err := r.Direct(f.Name)
	if err != nil {
		return nil, err
	}
	out := make([]cellar.Dependency, 0, len(order))
	for _, name := range order {
		d, ok := p.Index.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("%s: dependency %s: %w", f.Name, name, deps.ErrNotFound)
		}
		out = append(out, cellar.Dependency{
			FullName: d.FullName, Version: d.Versions.Stable, Revision: d.Revision,
			BottleRebuild: d.Bottle.Stable.Rebuild, PkgVersion: pkgVersion(d),
			DeclaredDirectly: slices.Contains(direct, d.Name),
		})
	}
	return out, nil
}

const versionedReason = ":versioned_formula"

var versionSuffix = regexp.MustCompile(`@[\d.]+(-full)?$`)

// unversioned is the name of the formula without its @version.
func unversioned(name string) string {
	return versionSuffix.ReplaceAllString(name, "$1")
}

// autoLinks says whether brew links f although it is keg-only: a versioned formula
// that was asked for by name is linked when no version of it and nothing related to
// it is installed, and the unversioned formula is not keg-only itself.
func (p *Planner) autoLinks(f *api.Formula, requested bool) bool {
	if !requested || f.KegOnlyReason.Reason != versionedReason {
		return false
	}
	if slices.ContainsFunc(p.Kegs, func(k cellar.Keg) bool { return k.Name == f.Name }) {
		return false
	}
	base := unversioned(f.Name)
	for _, other := range p.related(f) {
		if slices.ContainsFunc(p.Kegs, func(k cellar.Keg) bool { return k.Name == other.Name }) {
			return false
		}
		if other.Name == base && other.KegOnly {
			return false
		}
	}
	return true
}

// related lists the formulae that share links with f, as brew does: the other
// versions, the -full counterpart and the unversioned formula, and theirs in turn.
func (p *Planner) related(f *api.Formula) []*api.Formula {
	versions := map[string][]string{} // unversioned name to its versioned formulae
	for _, g := range p.Index.Formulae() {
		if strings.Contains(g.Name, "@") {
			base := unversioned(g.Name)
			versions[base] = append(versions[base], g.Name)
		}
	}
	siblings := func(name string) []string {
		out := slices.Clone(versions[unversioned(name)])
		if full, ok := strings.CutSuffix(name, "-full"); ok {
			out = append(out, full)
		} else {
			out = append(out, name+"-full")
		}
		if u := unversioned(name); u != name {
			out = append(out, u)
		}
		return out
	}
	seen := map[string]bool{f.Name: true}
	var out []*api.Formula
	queue := []string{f.Name}
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		for _, n := range siblings(name) {
			g, ok := p.Index.Lookup(n) // an alias leads to the formula it names
			if !ok || seen[g.Name] {
				continue
			}
			seen[g.Name] = true
			out = append(out, g)
			queue = append(queue, g.Name)
		}
	}
	return out
}

// hostArch is the name brew gives the CPU of the host.
func hostArch(h platform.Host) string {
	if h.Arch == "amd64" {
		return "x86_64"
	}
	return h.Arch
}

func pkgVersion(f *api.Formula) string {
	if f.Revision > 0 {
		return f.Versions.Stable + "_" + strconv.Itoa(f.Revision)
	}
	return f.Versions.Stable
}

// installed reports whether any version of f is in the Cellar. A formula of
// that name that came from another tap is an error.
func (p *Planner) installed(f *api.Formula) (bool, error) {
	var named []cellar.Keg
	for _, k := range p.Kegs {
		if k.Name == f.Name {
			named = append(named, k)
		}
	}
	if len(named) == 0 {
		return false, nil
	}
	if len(outdated.FromTap(named, f.Tap)) == 0 {
		return false, fmt.Errorf("%s: %w", f.Name, ErrOtherTap)
	}
	if p.upgrading[f.Name] {
		return false, nil
	}
	return true, nil
}

// check records what stops f being installed, and picks its bottle tag.
func (p *Planner) check(it *Item, problems *[]error) error {
	f := it.Formula
	if f.Disabled {
		*problems = append(*problems, fmt.Errorf("%s: %w", f.Name, ErrDisabled))
	} else if f.Deprecated {
		now := p.Now
		if now.IsZero() {
			now = time.Now()
		}
		it.Warnings = append(it.Warnings, f.Name+" is "+strings.ToLower(info.Deprecation(f, now)[:1])+info.Deprecation(f, now)[1:])
	}
	if u := poststep.Unsupported(f.PostInstallSteps); len(u) > 0 {
		*problems = append(*problems, fmt.Errorf("%s: %w (%s)", f.Name, ErrPostInstall, strings.Join(u, ", ")))
	}
	tag, ok := platform.Select(p.Host, f.BottleTags())
	if !ok {
		hostTag, _ := platform.Tag(p.Host)
		*problems = append(*problems, fmt.Errorf("%s: %w (%s)", f.Name, ErrNoBottle, hostTag))
	}
	it.Tag = tag
	for _, name := range f.ConflictsWith {
		other, ok := p.Index.Lookup(name)
		if !ok || other.Name == f.Name || !p.Linked(other.Name) {
			continue
		}
		*problems = append(*problems, fmt.Errorf("%s: %w: %s", f.Name, ErrConflict, other.Name))
	}
	return nil
}

// checkCellar refuses bottles that need work we cannot do for this prefix.
func (p *Planner) checkCellar(it *Item) error {
	name, cellar := it.Formula.Name, it.Bottle.Cellar
	if p.Host.OS == "linux" {
		return fmt.Errorf("%s: %w: its ELF files are not rewritten yet", name, ErrUnsupportedHost)
	}
	switch cellar {
	case ":any", ":any_skip_relocation", p.Prefix + "/Cellar":
		return nil
	}
	return fmt.Errorf("%s: %w (%s, this prefix is %s)", name, ErrOtherPrefix, cellar, p.Prefix)
}
