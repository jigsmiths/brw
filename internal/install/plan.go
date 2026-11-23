// Package install plans and carries out installs of bottles into a prefix.
package install

import (
	"encoding/json"
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
	"github.com/jigsmiths/brw/internal/version"
)

var (
	ErrNoBottle        = errors.New("no bottle for this host")
	ErrDisabled        = errors.New("disabled")
	ErrPostInstall     = errors.New("has post_install steps brw cannot run yet")
	ErrOtherPrefix     = errors.New("its bottle is built for another prefix")
	ErrUnsupportedHost = errors.New("cannot be installed on this host yet")
	ErrConflict        = errors.New("conflicts with a linked formula")
	ErrOtherTap        = errors.New("installed from another tap")
	ErrHeadCore        = errors.New("--HEAD builds from the formula's file, which brw reads only for tap formulae")
)

type BottleSource interface {
	Bottles(want map[string]string) (map[string]api.BottleFile, error)
}

type Item struct {
	Formula *api.Formula
	Tag     string
	HostTag string // differs from Tag for an "all" bottle
	Arch    string
	// RuntimeDeps nil leaves the bottle's own list in the receipt.
	RuntimeDeps []cellar.Dependency
	Bottle      api.BottleFile
	Build       bool
	BuildDeps   []string
	PkgVersion  string
	OnRequest   bool
	KegOnly     bool
	Warnings    []string

	// Link says whether to link the new keg: not keg-only, or for an upgrade what the old keg had.
	Link bool
	// Upgrade holds the kegs of the older versions this one replaces, unset for a fresh install.
	Upgrade   []cellar.Keg
	Reinstall bool
	// Head builds the formula's head spec, as install --HEAD does. PkgVersion
	// is "HEAD" until the installer fetches the commit it names the keg after.
	Head bool
}

// Locks holds the items and their runtime dependencies.
type Plan struct {
	Items            []Item
	Locks            []string
	AlreadyInstalled []string
	HeadInstalled    []string // full name and version of each --HEAD formula with a HEAD keg already
}

type Planner struct {
	Index     *api.Index
	Host      platform.Host
	Prefix    string
	Bottles   BottleSource
	Kegs      []cellar.Keg
	Linked    func(name string) bool
	Optlinked func(name string) bool
	Now       time.Time // zero means now

	// Head names the formulae, as given, to install from their head spec.
	Head map[string]bool

	upgrading map[string]bool // count as not installed
}

// Plan reports every problem found, not just the first.
func (p *Planner) Plan(names []string) (*Plan, error) {
	r, err := deps.NewResolver(p.Index, p.Host)
	if err != nil {
		return nil, err
	}
	hostTag, err := platform.Tag(p.Host)
	if err != nil {
		return nil, err
	}

	w := &planWalk{p: p, r: r, hostTag: hostTag, plan: &Plan{}, visited: map[string]bool{}, skipped: map[string]bool{}}
	for _, name := range names {
		f, ok := p.Index.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("%s: %w", name, deps.NotFound(p.Index, name))
		}
		if p.Head[name] {
			done, err := w.head(f)
			if err != nil {
				return nil, err
			}
			if done {
				continue
			}
			hf := f.HeadSpec()
			f = &hf
			r.Use(f)
			w.heads = append(w.heads, f.Name)
		}
		if err := w.visit(f, true); err != nil {
			return nil, err
		}
	}
	plan, problems := w.plan, w.problems
	if err := p.attachBottles(plan, &problems); err != nil {
		return nil, err
	}
	if err := errors.Join(problems...); err != nil {
		return nil, err
	}
	if plan.Locks, err = lockNames(r, plan.Items); err != nil {
		return nil, err
	}
	return plan, nil
}

type planWalk struct {
	p        *Planner
	r        *deps.Resolver
	hostTag  string
	plan     *Plan
	problems []error
	visited  map[string]bool // set before the dependencies are walked, so a cycle ends
	skipped  map[string]bool
	heads    []string // formulae to build from their head spec
}

// head checks a formula asked for with --HEAD as brew does. done says there
// is nothing to install: a HEAD keg is there already.
func (w *planWalk) head(f *api.Formula) (done bool, err error) {
	p := w.p
	if f.Versions.Head == "" {
		return false, fmt.Errorf("No head is defined for %s", f.FullName) //nolint:staticcheck // brew's message
	}
	if !f.FromTap() {
		return false, fmt.Errorf("%s: %w", f.FullName, ErrHeadCore)
	}
	if p.Host.OS != "darwin" {
		return false, fmt.Errorf("%s: %w: brw builds on macOS only", f.Name, ErrUnsupportedHost)
	}
	var linked string
	for _, k := range p.Kegs {
		if k.Name != f.Name {
			continue
		}
		if pv, err := version.ParsePkg(k.Version); err == nil && pv.Version.IsHead() {
			w.plan.HeadInstalled = append(w.plan.HeadInstalled, f.FullName+" "+k.Version)
			return true, nil
		}
		if p.Linked(f.Name) {
			linked = k.Version
		}
	}
	if linked != "" {
		return false, fmt.Errorf("%s %s is already installed\nTo install HEAD, first run:\n  brw unlink %s", f.Name, linked, f.Name)
	}
	return false, nil
}

func (w *planWalk) visit(f *api.Formula, requested bool) error {
	p, r := w.p, w.r
	if w.visited[f.Name] {
		return nil
	}
	head := requested && slices.Contains(w.heads, f.Name)
	if installed, err := p.installed(f); err != nil {
		w.problems = append(w.problems, err)
		return nil
	} else if installed && !head {
		if requested && !w.skipped[f.Name] {
			w.plan.AlreadyInstalled = append(w.plan.AlreadyInstalled, f.Name)
		}
		w.skipped[f.Name] = true
		return nil
	}
	w.visited[f.Name] = true
	direct, err := r.Direct(f.Name)
	if err != nil {
		return err
	}
	if head || p.builds(f) {
		direct = append(slices.Clone(direct), f.BuildDependencies...)
	}
	for _, d := range direct {
		df, ok := p.Index.Lookup(d)
		if !ok {
			return fmt.Errorf("%s: dependency %s: %w", f.Name, d, deps.NotFound(p.Index, d))
		}
		if err := w.visit(df, false); err != nil {
			return err
		}
	}
	g, err := f.ForTag(w.hostTag)
	if err != nil {
		return err
	}
	runtime, err := p.runtimeDeps(r, f)
	if err != nil {
		return err
	}
	it := Item{Formula: f, HostTag: w.hostTag, Arch: hostArch(p.Host), RuntimeDeps: runtime, OnRequest: requested, KegOnly: g.KegOnly && !p.autoLinks(f, requested), PkgVersion: pkgVersion(f)}
	it.Link = !it.KegOnly
	if head {
		it.Head, it.PkgVersion = true, withRevision("HEAD", f.Revision)
	}
	if head || p.builds(f) {
		it.BuildDeps = f.BuildDependencies
	}
	w.plan.Items = append(w.plan.Items, it)
	return nil
}

func (p *Planner) attachBottles(plan *Plan, problems *[]error) error {
	want := map[string]string{}
	for i := range plan.Items {
		it := &plan.Items[i]
		p.check(it, problems)
		if it.Tag != "" && !it.Formula.FromTap() {
			want[it.Formula.Name] = it.Tag
		}
	}
	files, err := p.bottles(want)
	if err != nil {
		return err
	}
	for i := range plan.Items {
		it := &plan.Items[i]
		if it.Tag == "" {
			continue
		}
		it.Bottle = files[it.Formula.Name]
		if it.Formula.FromTap() {
			if it.Bottle, err = p.Index.TapBottle(it.Formula.FullName, it.Tag); err != nil {
				return err
			}
		}
		if err := p.checkCellar(it); err != nil {
			*problems = append(*problems, err)
		}
	}
	return nil
}

// lockNames includes installed runtime dependencies, which must not change while the items go in on top.
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

// PlanUpgrade keeps each formula's on-request and linked state and puts keg-only formulae first.
// A formula that cannot be upgraded is reported and does not stop the others.
func (p *Planner) PlanUpgrade(names []string) (*Plan, []error) {
	return p.planInstalled(names, false)
}

// PlanReinstall keeps each formula's on-request and linked state; a name that is not installed is a plain install.
func (p *Planner) PlanReinstall(names []string) (*Plan, []error) {
	return p.planInstalled(names, true)
}

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

// PlanEach reports a name that cannot be planned without stopping the others.
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
		merged.HeadInstalled = append(merged.HeadInstalled, one.HeadInstalled...)
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
		// upgrade.rb: an opt-linked keg keeps its link state, else link unless keg-only.
		if p.Optlinked(it.Formula.Name) {
			it.Link = p.Linked(it.Formula.Name)
		}
	}
}

func (p *Planner) bottles(want map[string]string) (map[string]api.BottleFile, error) {
	if len(want) == 0 {
		return nil, nil
	}
	return p.Bottles.Bottles(want)
}

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
			return nil, fmt.Errorf("%s: dependency %s: %w", f.Name, name, deps.NotFound(p.Index, name))
		}
		dep := cellar.Dependency{
			FullName: d.FullName, Version: d.Versions.Stable, Revision: d.Revision,
			BottleRebuild: d.Bottle.Stable.Rebuild, PkgVersion: pkgVersion(d),
			DeclaredDirectly: slices.Contains(direct, d.Name),
		}
		if d.CompatibilityVersion > 0 {
			dep.CompatibilityVersion = json.RawMessage(strconv.Itoa(d.CompatibilityVersion))
		}
		out = append(out, dep)
	}
	return out, nil
}

const versionedReason = ":versioned_formula"

var versionSuffix = regexp.MustCompile(`@[\d.]+(-full)?$`)

func unversioned(name string) string {
	return versionSuffix.ReplaceAllString(name, "$1")
}

// autoLinks: a versioned keg-only formula asked for by name is linked when
// nothing related is installed and the unversioned formula is not keg-only.
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

// related: other versions, the -full counterpart and the unversioned formula, transitively.
func (p *Planner) related(f *api.Formula) []*api.Formula {
	versions := map[string][]string{}
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
			g, ok := p.Index.Lookup(n)
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

func hostArch(h platform.Host) string {
	if h.Arch == "amd64" {
		return "x86_64"
	}
	return h.Arch
}

func pkgVersion(f *api.Formula) string { return withRevision(f.Versions.Stable, f.Revision) }

// withRevision is version with the revision, as brew's PkgVersion writes it.
func withRevision(version string, revision int) string {
	if revision > 0 {
		return version + "_" + strconv.Itoa(revision)
	}
	return version
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

// builds: a tap formula with no bottle for this host; macOS only so far.
func (p *Planner) builds(f *api.Formula) bool {
	_, bottled := platform.Select(p.Host, f.BottleTags())
	return !bottled && f.FromTap() && f.URLs.Stable.URL != "" && p.Host.OS == "darwin"
}

func (p *Planner) check(it *Item, problems *[]error) {
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
	switch {
	case it.Head || p.builds(f):
		it.Build = true
	case !ok:
		hostTag, _ := platform.Tag(p.Host)
		*problems = append(*problems, fmt.Errorf("%s: %w (%s)", f.FullName, ErrNoBottle, hostTag))
	}
	if !it.Head { // a HEAD build uses no bottle
		it.Tag = tag
	}
	for _, name := range f.ConflictsWith {
		other, ok := p.Index.Lookup(name)
		if !ok || other.Name == f.Name || !p.Linked(other.Name) {
			continue
		}
		*problems = append(*problems, fmt.Errorf("%s: %w: %s", f.Name, ErrConflict, other.Name))
	}
}

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
