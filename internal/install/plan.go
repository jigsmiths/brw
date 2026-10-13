// Package install plans and carries out installs of bottles into a prefix.
package install

import (
	"errors"
	"fmt"
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
	Formula    *api.Formula
	Tag        string
	Bottle     api.BottleFile
	PkgVersion string // version and revision, which names the keg directory
	OnRequest  bool   // asked for by name, not only needed by something else
	KegOnly    bool
	Warnings   []string

	// Upgrade holds the kegs of the older versions this one replaces, and Linked
	// whether to link the new keg, which an upgrade takes from the old one. Both
	// are unset for a fresh install.
	Upgrade []cellar.Keg
	Linked  *bool
	// Reinstall says the old keg is put aside and the formula installed afresh,
	// and the old keg is deleted once that worked.
	Reinstall bool
}

// Plan lists what to install, dependencies first.
type Plan struct {
	Items            []Item
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
		if i, ok := position[f.Name]; ok {
			plan.Items[i].OnRequest = plan.Items[i].OnRequest || requested
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
		plan.Items = append(plan.Items, Item{Formula: f, OnRequest: requested, KegOnly: g.KegOnly, PkgVersion: pkgVersion(f)})
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
	return plan, nil
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
	merged := &Plan{}
	var errs []error
	for _, n := range ordered {
		one, err := p.Plan([]string{n})
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, it := range one.Items {
			if !slices.ContainsFunc(merged.Items, func(x Item) bool { return x.Formula.Name == it.Formula.Name }) {
				merged.Items = append(merged.Items, it)
			}
		}
	}
	p.markUpgrades(merged, reinstall)
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
		linked := p.Linked(it.Formula.Name)
		it.Linked = &linked
	}
}

func (p *Planner) bottles(want map[string]string) (map[string]api.BottleFile, error) {
	if len(want) == 0 {
		return nil, nil
	}
	return p.Bottles.Bottles(want)
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
	if p.Host.OS == "linux" && cellar != ":any_skip_relocation" {
		return fmt.Errorf("%s: %w: it needs its ELF files rewritten", name, ErrUnsupportedHost)
	}
	switch cellar {
	case ":any", ":any_skip_relocation", p.Prefix + "/Cellar":
		return nil
	}
	return fmt.Errorf("%s: %w (%s, this prefix is %s)", name, ErrOtherPrefix, cellar, p.Prefix)
}
