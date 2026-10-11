// Package deps resolves formula dependencies from the API and from receipts.
package deps

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/platform"
)

var (
	ErrNotFound     = errors.New("no available formula")
	ErrNotInstalled = errors.New("not installed")
)

// Resolver reads declared dependencies for one host.
type Resolver struct {
	idx      *api.Index
	tag      string
	linux    bool
	macMajor int
	reverse  map[string][]string
}

func NewResolver(idx *api.Index, host platform.Host) (*Resolver, error) {
	tag, err := platform.Tag(host)
	if err != nil {
		return nil, err
	}
	r := &Resolver{idx: idx, tag: tag, linux: host.OS == "linux"}
	if !r.linux {
		if r.macMajor, err = platform.MacMajor(host.MacVersion); err != nil {
			return nil, err
		}
	}
	return r, nil
}

func (r *Resolver) formula(name string) (*api.Formula, error) {
	f, ok := r.idx.Lookup(name)
	if !ok {
		return nil, fmt.Errorf("%s: %w", name, ErrNotFound)
	}
	return f, nil
}

// names lists f's dependency names as written, for this host.
func (r *Resolver) names(f *api.Formula) ([]string, error) {
	g, err := f.ForTag(r.tag)
	if err != nil {
		return nil, err
	}
	names := slices.Clone(g.Dependencies)
	for _, u := range g.UsesFromMacOS {
		if u.Runtime() && r.needsReal(u) {
			names = append(names, u.Name)
		}
	}
	return names, nil
}

// needsReal reports whether the host lacks a system copy of u. Linux has none.
// An unknown release name counts as provided.
func (r *Resolver) needsReal(u api.UsesFromMacOS) bool {
	if r.linux {
		return true
	}
	since, ok := platform.ReleaseMajor(u.Since)
	return ok && r.macMajor < since
}

// declared returns name's direct dependencies as canonical names, in the order
// they are declared.
func (r *Resolver) declared(name string) ([]string, error) {
	f, err := r.formula(name)
	if err != nil {
		return nil, err
	}
	names, err := r.names(f)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		d, ok := r.idx.Lookup(n)
		if !ok {
			return nil, fmt.Errorf("dependency %q of %s: %w", n, f.Name, ErrNotFound)
		}
		if !slices.Contains(out, d.Name) {
			out = append(out, d.Name)
		}
	}
	return out, nil
}

// Direct returns the sorted, canonical names of name's direct dependencies.
func (r *Resolver) Direct(name string) ([]string, error) {
	out, err := r.declared(name)
	slices.Sort(out)
	return out, err
}

// Recursive returns every dependency of name, sorted, without name itself.
func (r *Resolver) Recursive(name string) ([]string, error) {
	root, err := r.formula(name)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{root.Name: true}
	queue := []string{root.Name}
	var out []string
	for len(queue) > 0 {
		deps, err := r.Direct(queue[0])
		if err != nil {
			return nil, err
		}
		queue = queue[1:]
		for _, d := range deps {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
				queue = append(queue, d)
			}
		}
	}
	slices.Sort(out)
	return out, nil
}

// Tree draws name's dependencies. Shared subtrees are repeated in full.
func (r *Resolver) Tree(name string) (string, error) {
	root, err := r.formula(name)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(root.Name + "\n")
	if err := r.draw(&b, root.Name, "", []string{root.Name}); err != nil {
		return "", err
	}
	return b.String(), nil
}

func (r *Resolver) draw(b *strings.Builder, name, prefix string, path []string) error {
	children, err := r.declared(name)
	if err != nil {
		return err
	}
	for i, c := range children {
		if slices.Contains(path, c) {
			return fmt.Errorf("dependency cycle: %s -> %s", strings.Join(path, " -> "), c)
		}
		connector, indent := "├── ", "│   "
		if i == len(children)-1 {
			connector, indent = "└── ", "    "
		}
		b.WriteString(prefix + connector + c + "\n")
		if err := r.draw(b, c, prefix+indent, append(path, c)); err != nil {
			return err
		}
	}
	return nil
}

// Uses returns the formulae that depend on name, sorted. With recursive it
// includes dependents of dependents. Unresolvable dependencies are ignored.
func (r *Resolver) Uses(name string, recursive bool) ([]string, error) {
	target, err := r.formula(name)
	if err != nil {
		return nil, err
	}
	if r.reverse == nil {
		r.reverse = r.buildReverse()
	}
	seen := map[string]bool{target.Name: true}
	queue := []string{target.Name}
	var out []string
	for len(queue) > 0 {
		for _, u := range r.reverse[queue[0]] {
			if !seen[u] {
				seen[u] = true
				out = append(out, u)
				if recursive {
					queue = append(queue, u)
				}
			}
		}
		queue = queue[1:]
	}
	slices.Sort(out)
	return out, nil
}

func (r *Resolver) buildReverse() map[string][]string {
	reverse := map[string][]string{}
	for _, f := range r.idx.Formulae() {
		names, err := r.names(f)
		if err != nil {
			continue
		}
		for _, n := range names {
			if d, ok := r.idx.Lookup(n); ok {
				reverse[d.Name] = append(reverse[d.Name], f.Name)
			}
		}
	}
	return reverse
}

// Installed returns the runtime dependencies recorded in name's receipt, sorted.
func Installed(kegs []cellar.Keg, name string) ([]string, error) {
	var keg *cellar.Keg
	for i := range kegs {
		if kegs[i].Name == name {
			keg = &kegs[i]
		}
	}
	if keg == nil {
		return nil, fmt.Errorf("%s: %w", name, ErrNotInstalled)
	}
	if keg.Receipt == nil {
		return nil, fmt.Errorf("%s %s has no install receipt", name, keg.Version)
	}
	out := []string{}
	for _, d := range keg.Receipt.RuntimeDependencies {
		out = append(out, d.FullName)
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// InstalledUses returns the installed formulae whose receipts list name as a
// runtime dependency, direct or not. Formulae from taps other than core are
// named in full, and the list is sorted by those names.
func InstalledUses(kegs []cellar.Keg, name string) []string {
	var out []string
	for _, k := range kegs {
		if k.Receipt == nil || k.Name == name {
			continue
		}
		for _, d := range k.Receipt.RuntimeDependencies {
			if d.FullName == name {
				out = append(out, fullName(k))
				break
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func fullName(k cellar.Keg) string {
	if tap := k.Receipt.Source.Tap; tap != "" && tap != "homebrew/core" {
		return tap + "/" + k.Name
	}
	return k.Name
}
