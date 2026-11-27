package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cask"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/deps"
	"github.com/jigsmiths/brw/internal/tap"
)

type depSource struct {
	cask        func(token string) (api.Cask, error)
	formulaDeps func(name string) ([]string, error)
	formula     func(name string) (cellar.Dependency, error)
}

type depNode struct {
	cask bool
	name string
}

func (n depNode) shortName() string { return path.Base(n.name) }

// depGraph mirrors brew's TopologicalHash: each package maps to its dependencies, formulae first, in the order first reached.
type depGraph struct {
	root     depNode
	order    []depNode
	children map[depNode][]depNode
}

func buildGraph(root api.Cask, src depSource) (depGraph, error) {
	g := depGraph{root: depNode{cask: true, name: root.Token}, children: map[depNode][]depNode{}}
	var visit func(n depNode) error
	visit = func(n depNode) error {
		if _, seen := g.children[n]; seen {
			return nil
		}
		var kids []depNode
		if n.cask {
			c := root
			if n != g.root {
				var err error
				if c, err = src.cask(n.name); err != nil {
					return err
				}
			}
			for _, f := range c.DependsOn.Formula {
				kids = append(kids, depNode{name: f})
			}
			for _, k := range c.DependsOn.Cask {
				kids = append(kids, depNode{cask: true, name: k})
			}
		} else {
			names, err := src.formulaDeps(n.name)
			if err != nil {
				return err
			}
			for _, f := range names {
				kids = append(kids, depNode{name: f})
			}
		}
		g.children[n] = kids
		g.order = append(g.order, n)
		for _, k := range kids {
			if err := visit(k); err != nil {
				return err
			}
		}
		return nil
	}
	return g, visit(g.root)
}

func (g depGraph) tsort() ([]depNode, error) {
	token := g.root.name
	if slices.Contains(g.children[g.root], g.root) {
		return nil, fmt.Errorf("Cask '%s' depends on itself.", token) //nolint:staticcheck // brew's message
	}
	number := map[depNode]int{}
	low := map[depNode]int{}
	var stack []depNode
	onStack := map[depNode]bool{}
	var components [][]depNode
	var strong func(n depNode)
	strong = func(n depNode) {
		number[n] = len(number) + 1
		low[n] = number[n]
		stack = append(stack, n)
		onStack[n] = true
		for _, k := range g.children[n] {
			if number[k] == 0 {
				strong(k)
				low[n] = min(low[n], low[k])
			} else if onStack[k] {
				low[n] = min(low[n], number[k])
			}
		}
		if low[n] != number[n] {
			return
		}
		i := slices.Index(stack, n)
		component := slices.Clone(stack[i:])
		for _, m := range component {
			onStack[m] = false
		}
		stack = stack[:i]
		components = append(components, component)
	}
	for _, n := range g.order {
		if number[n] == 0 {
			strong(n)
		}
	}
	var out, cycle []depNode
	for _, c := range components {
		out = append(out, c...)
		if len(c) > 1 && len(c) >= len(cycle) {
			cycle = c
		}
	}
	if len(cycle) > 0 {
		var names []string
		for _, n := range cycle {
			if n != g.root {
				names = append(names, n.name)
			}
		}
		return nil, fmt.Errorf("Cask '%s' includes cyclic dependencies on other Casks: %s", token, listSentence(names)) //nolint:staticcheck // brew's message
	}
	return slices.DeleteFunc(out, func(n depNode) bool { return n == g.root }), nil
}

// runtimeDeps orders dependencies as brew's Cask::Tab does: each package's dependencies as first reached.
func runtimeDeps(root api.Cask, src depSource) (cask.RuntimeDependencies, error) {
	g, err := buildGraph(root, src)
	if err != nil {
		return cask.RuntimeDependencies{}, err
	}
	var out cask.RuntimeDependencies
	var seen []depNode
	for _, n := range g.order {
		for _, k := range g.children[n] {
			if k == g.root || slices.Contains(seen, k) {
				continue
			}
			seen = append(seen, k)
			if k.cask {
				c, err := src.cask(k.name)
				if err != nil {
					return out, err
				}
				out.Cask = append(out.Cask, cask.CaskDependency{
					FullName: k.name, Version: c.Version, DeclaredDirectly: slices.Contains(root.DependsOn.Cask, k.name),
				})
				continue
			}
			d, err := src.formula(k.name)
			if err != nil {
				return out, err
			}
			d.DeclaredDirectly = slices.Contains(root.DependsOn.Formula, d.FullName)
			out.Formula = append(out.Formula, d)
		}
	}
	return out, nil
}

type depLoader struct {
	e        *env
	ctx      context.Context
	prefix   string
	tag      string
	casks    *api.CaskIndex
	idx      *api.Index
	resolver *deps.Resolver
	autoTap  bool
}

func (l *depLoader) formulae() (*api.Index, *deps.Resolver, error) {
	if l.resolver != nil {
		return l.idx, l.resolver, nil
	}
	if l.idx == nil {
		var err error
		if l.idx, _, err = l.e.indexAndClient(l.ctx, l.prefix); err != nil {
			return nil, nil, err
		}
	}
	r, err := deps.NewResolver(l.idx, l.e.host)
	l.resolver = r
	return l.idx, r, err
}

func (l *depLoader) tapMissing(name string) (tapped bool, err error) {
	tapName, _, ok := tap.Split(name)
	if !l.autoTap || !ok || strings.HasPrefix(tapName, "homebrew/") {
		return false, nil
	}
	if _, err := os.Stat(tap.Dir(l.prefix, tapName)); err == nil {
		return false, nil
	}
	_, err = l.e.ensureTap(l.ctx, l.prefix, tapName)
	return err == nil, err
}

func (l *depLoader) source(owner string) depSource {
	return depSource{
		cask: func(token string) (api.Cask, error) {
			if tapped, err := l.tapMissing(token); err != nil {
				return api.Cask{}, err
			} else if tapped {
				var err error
				if l.casks, err = l.e.caskIndex(l.ctx, l.prefix); err != nil {
					return api.Cask{}, err
				}
			}
			k, ok := l.casks.Lookup(token)
			if !ok {
				return api.Cask{}, fmt.Errorf("Cask '%s' is unavailable: No Cask with this name exists.", token) //nolint:staticcheck // brew's message
			}
			return k.ForTag(l.tag)
		},
		formulaDeps: func(name string) ([]string, error) {
			if tapped, err := l.tapMissing(name); err != nil {
				return nil, err
			} else if tapped {
				l.idx, l.resolver = nil, nil
			}
			idx, r, err := l.formulae()
			if err != nil {
				return nil, err
			}
			names, err := r.Declared(name)
			if err != nil {
				return nil, err
			}
			full := make([]string, len(names))
			for i, n := range names {
				d, _ := idx.Lookup(n)
				full[i] = d.FullName
			}
			return full, nil
		},
		formula: func(name string) (cellar.Dependency, error) {
			idx, _, err := l.formulae()
			if err != nil {
				return cellar.Dependency{}, err
			}
			d, ok := idx.Lookup(name)
			if !ok {
				return cellar.Dependency{}, fmt.Errorf("%s needs the formula %s: %w", owner, name, deps.NotFound(idx, name))
			}
			dep := cellar.Dependency{
				FullName: d.FullName, Version: d.Versions.Stable, Revision: d.Revision,
				BottleRebuild: d.Bottle.Stable.Rebuild, PkgVersion: pkgVersionOf(d.Versions.Stable, d.Revision),
			}
			if d.CompatibilityVersion > 0 {
				dep.CompatibilityVersion = json.RawMessage(strconv.Itoa(d.CompatibilityVersion))
			}
			return dep, nil
		},
	}
}

func (e *env) runtimeDepsFor(ctx context.Context, prefix, tag string, casks *api.CaskIndex, idx *api.Index) func(api.Cask) (cask.RuntimeDependencies, error) {
	l := &depLoader{e: e, ctx: ctx, prefix: prefix, tag: tag, casks: casks, idx: idx}
	return func(c api.Cask) (cask.RuntimeDependencies, error) {
		if len(c.DependsOn.Formula) == 0 && len(c.DependsOn.Cask) == 0 {
			return cask.RuntimeDependencies{}, nil
		}
		return runtimeDeps(c, l.source(c.Token))
	}
}

func installedDependency(prefix string, installedCasks map[string]bool, n depNode) bool {
	if n.cask {
		return installedCasks[n.shortName()]
	}
	_, ok := cellar.Prefix(prefix).OptVersion(n.shortName())
	return ok
}

func installedCaskTokens(prefix string) (map[string]bool, error) {
	have, err := caskroom.Installed(prefix)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, c := range have {
		out[c.Token] = true
	}
	return out, nil
}

// satisfyDependencies installs missing casks, then formulae, as brew does, none on request.
func (e *env) satisfyDependencies(ctx context.Context, prefix, journal string, parent *cask.Installer, where cask.Where, config cask.Config, tag string, casks *api.CaskIndex, c api.Cask) error {
	l := &depLoader{e: e, ctx: ctx, prefix: prefix, tag: tag, casks: casks, autoTap: true}
	src := l.source(c.Token)
	g, err := buildGraph(c, src)
	if err != nil {
		return fmt.Errorf("%s: %w", c.Token, err)
	}
	order, err := g.tsort()
	if err != nil {
		return fmt.Errorf("%s: %w", c.Token, err)
	}
	if len(order) == 0 {
		return nil
	}
	installed, err := installedCaskTokens(prefix)
	if err != nil {
		return err
	}
	missing := slices.DeleteFunc(slices.Clone(order), func(n depNode) bool { return installedDependency(prefix, installed, n) })
	if len(missing) == 0 {
		_, _ = fmt.Fprintln(e.out, "All dependencies satisfied.")
		return nil
	}
	names := make([]string, len(missing))
	for i, n := range missing {
		names[i] = n.shortName()
	}
	_, _ = fmt.Fprintf(e.out, "==> Installing dependencies: %s\n", strings.Join(names, ", "))
	var formulae []string
	for _, n := range missing {
		if !n.cask {
			formulae = append(formulae, n.name)
			continue
		}
		dep, err := src.cask(n.name)
		if err != nil {
			return err
		}
		if err := installDependencyCask(ctx, parent, where, config, dep); err != nil {
			return err
		}
	}
	if len(formulae) == 0 {
		return nil
	}
	r := resolved{formulaDeps: formulae}
	if r.client, err = e.apiClient(); err != nil {
		return err
	}
	idx, _, err := l.formulae()
	if err != nil {
		return err
	}
	r.idx = idx
	if err := e.tapMissing(ctx, prefix, &r, formulae); err != nil {
		return err
	}
	return installFormulae(e, prefix, journal, false, false, r)
}

func installDependencyCask(ctx context.Context, parent *cask.Installer, where cask.Where, config cask.Config, dep api.Cask) error {
	in := *parent
	in.SatisfyDependencies, in.Where, in.Config, in.Force = nil, where, config, false
	return in.InstallAsDependency(ctx, dep)
}

func (e *env) dryRunDependencies(ctx context.Context, prefix, tag string, casks *api.CaskIndex, c api.Cask) ([]string, error) {
	l := &depLoader{e: e, ctx: ctx, prefix: prefix, tag: tag, casks: casks}
	g, err := buildGraph(c, l.source(c.Token))
	if err != nil {
		return nil, err
	}
	order, err := g.tsort()
	if err != nil {
		return nil, err
	}
	installed, err := installedCaskTokens(prefix)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, n := range order {
		if n.cask && !installedDependency(prefix, installed, n) {
			names = append(names, n.name)
		}
	}
	if len(c.DependsOn.Formula) > 0 {
		_, r, err := l.formulae()
		if err != nil {
			return nil, err
		}
		for _, f := range c.DependsOn.Formula {
			sub, err := r.Expanded(f)
			if err != nil {
				return nil, err
			}
			for _, name := range append([]string{f}, sub...) {
				n := depNode{name: name}
				if !installedDependency(prefix, installed, n) && !slices.Contains(names, name) {
					names = append(names, name)
				}
			}
		}
	}
	return names, nil
}

// listSentence joins words as brew's to_sentence does, which differs from sentence in the final comma.
func listSentence(words []string) string {
	switch len(words) {
	case 0, 1:
		return strings.Join(words, "")
	case 2:
		return words[0] + " and " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", and " + words[len(words)-1]
}
