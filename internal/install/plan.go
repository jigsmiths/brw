// Package install plans and carries out installs of bottles into a prefix.
package install

import (
	"errors"
	"fmt"
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
	if len(f.Service) > 0 && string(f.Service) != "null" {
		it.Warnings = append(it.Warnings, f.Name+" has a service definition, and brw does not write its launchd or systemd files")
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
