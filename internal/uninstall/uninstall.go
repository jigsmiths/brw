// Package uninstall removes kegs the way brew uninstall does.
package uninstall

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/racks"
)

// NoSuchKegError and UnavailableError are what racks.Finder reports.
type (
	NoSuchKegError   = racks.NoSuchKegError
	UnavailableError = racks.UnavailableError
)

// DependentsError says which installed formulae need the kegs.
type DependentsError struct {
	Kegs, Dependents, Named []string
}

func (e *DependentsError) Error() string {
	one := len(e.Kegs) == 1
	return fmt.Sprintf("refusing to uninstall %s\nbecause %s required by %s, which %s currently installed.\n"+
		"You can override this and force removal with:\n  brw uninstall --ignore-dependencies %s",
		sentence(e.Kegs), pick(one, "it is", "they are"), sentence(e.Dependents),
		pick(len(e.Dependents) == 1, "is", "are"), strings.Join(e.Named, " "))
}

func pick(cond bool, a, b string) string {
	if cond {
		return a
	}
	return b
}

// sentence joins words like Utils::Text.to_sentence: "a", "a and b", "a, b, and c".
func sentence(words []string) string {
	switch len(words) {
	case 0:
		return ""
	case 1:
		return words[0]
	case 2:
		return words[0] + " and " + words[1]
	}
	return strings.Join(words[:len(words)-1], ", ") + ", and " + words[len(words)-1]
}

type Options struct {
	Force, IgnoreDependencies bool
	Autoremove                bool // afterwards, remove dependencies nothing needs any more
}

type Remover struct {
	Root   *fsx.Root
	Prefix string // absolute path of Root
	Linker *link.Linker
	Kegs   []cellar.Keg // every installed keg

	// Resolve turns an alias into a formula name and says whether the formula
	// exists. It may be nil, as may FormulaNames, when no listing is at hand.
	Resolve      func(name string) (canonical string, known bool)
	FormulaNames func() map[string]bool
	// BuildDeps lists the build dependencies of a formula, for the ones that
	// were built from source. It may be nil.
	BuildDeps func(name string) []string

	Out, Warn io.Writer
	DryRun    bool
}

func (r *Remover) cellar() cellar.Prefix { return cellar.Prefix(r.Prefix) }

func (r *Remover) printf(w io.Writer, format string, args ...any) {
	if w != nil {
		_, _ = fmt.Fprintf(w, format, args...)
	}
}

func (r *Remover) warn(format string, args ...any) {
	r.printf(r.Warn, "brw: warning: "+format+"\n", args...)
}

func (r *Remover) rackKegs(name string) []cellar.Keg {
	var out []cellar.Keg
	for _, k := range r.Kegs {
		if k.Name == name {
			out = append(out, k)
		}
	}
	return out
}

func (r *Remover) rackNames() []string {
	var names []string
	for _, k := range r.Kegs {
		if !slices.Contains(names, k.Name) {
			names = append(names, k.Name)
		}
	}
	return names
}

func (r *Remover) resolve(name string) (string, error) {
	return racks.Finder{Prefix: r.Prefix, Kegs: r.Kegs, Resolve: r.Resolve}.Find(name)
}

type rack struct {
	name string
	kegs []cellar.Keg
}

// Run uninstalls the named formulae. Nothing is removed if an installed formula
// needs one of them, unless the option says otherwise.
func (r *Remover) Run(names []string, o Options) error {
	var problems []error
	var racks []rack
	for _, n := range names {
		name, err := r.resolve(n)
		if err != nil {
			problems = append(problems, err)
			continue
		}
		if slices.ContainsFunc(racks, func(x rack) bool { return x.name == name }) {
			continue
		}
		kegs := r.rackKegs(name)
		if !o.Force {
			kegs = []cellar.Keg{r.cellar().DefaultKeg(kegs)}
		}
		racks = append(racks, rack{name, kegs})
	}
	if !o.IgnoreDependencies {
		var all []cellar.Keg
		for _, x := range racks {
			all = append(all, x.kegs...)
		}
		if err := r.checkDependents(all, names); err != nil {
			return err
		}
	}
	var failed []error
	for _, x := range racks {
		if err := r.removeRack(x, o); err != nil {
			failed = append(failed, err)
		}
	}
	failed = append(failed, r.dropDanglingRacks())
	if o.Autoremove {
		failed = append(failed, r.Autoremove())
	}
	if !o.Force {
		failed = append(failed, problems...)
	}
	return errors.Join(failed...)
}

func (r *Remover) kegPath(k cellar.Keg) string {
	return path.Join(r.Prefix, "Cellar", k.Name, k.Version)
}

func (r *Remover) abv(p string) string {
	s, err := cellar.Keg{Path: p}.Abv()
	if err != nil {
		return "?"
	}
	return s
}

func (r *Remover) removeRack(x rack, o Options) (err error) {
	if r.DryRun {
		for _, k := range x.kegs {
			r.printf(r.Out, "Would uninstall %s... (%s)\n", r.kegPath(k), r.abv(k.Path))
		}
		r.forget(x.kegs)
		return nil
	}
	l, err := lock.Acquire(r.Root, x.name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, l.Release()) }()

	if o.Force {
		r.printf(r.Out, "Uninstalling %s... (%s)\n", x.name, r.abv(filepath.Join(r.Prefix, "Cellar", x.name)))
		for _, k := range x.kegs {
			if err := r.remove(k); err != nil {
				return err
			}
		}
		r.forget(x.kegs)
		return r.unpin(x.name)
	}

	k := x.kegs[0]
	if _, pinned := r.cellar().Pinned(x.name); pinned {
		r.printf(r.Warn, "brw: %s is pinned. You must unpin it to uninstall.\n", x.name)
		return nil
	}
	r.printf(r.Out, "Uninstalling %s... (%s)\n", r.kegPath(k), r.abv(k.Path))
	if err := r.remove(k); err != nil {
		return err
	}
	r.forget(x.kegs)
	if err := r.unpin(x.name); err != nil {
		return err
	}
	if left := r.versionsLeft(x.name); len(left) > 0 {
		r.printf(r.Out, "%s %s %s still installed.\nTo remove all versions, run:\n  brw uninstall --force %s\n",
			x.name, sentence(left), pick(len(left) == 1, "is", "are"), x.name)
	}
	r.configNotices(x.name)
	return nil
}

// forget drops removed kegs from the list of installed ones.
func (r *Remover) forget(kegs []cellar.Keg) {
	r.Kegs = slices.DeleteFunc(r.Kegs, func(k cellar.Keg) bool {
		return slices.ContainsFunc(kegs, func(x cellar.Keg) bool { return x.Path == k.Path })
	})
}

func (r *Remover) remove(k cellar.Keg) error {
	lk := link.Keg{Name: k.Name, Version: k.Version}
	if k.Receipt != nil {
		lk.Aliases = k.Receipt.Aliases
	}
	if err := r.Linker.Unlink(lk); err != nil {
		return err
	}
	if err := r.Root.RemoveKeg(path.Join("Cellar", k.Name, k.Version)); err != nil {
		return err
	}
	return r.Linker.Forget(lk)
}

func (r *Remover) versionsLeft(name string) []string {
	entries, err := r.Root.ReadDir(path.Join("Cellar", name))
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// unpin removes the pin record, if any, and the pin directory once it is empty.
func (r *Remover) unpin(name string) error {
	record := path.Join("var/homebrew/pinned", name)
	if fi, err := r.Root.Lstat(record); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		if err := r.Root.Remove(record); err != nil {
			return err
		}
	}
	_ = r.Root.Remove("var/homebrew/pinned") // only succeeds when empty
	return nil
}

// dropDanglingRacks removes Cellar entries that are links to nothing, which a
// renamed formula leaves behind.
func (r *Remover) dropDanglingRacks() error {
	entries, err := r.Root.ReadDir("Cellar")
	if err != nil {
		return nil
	}
	var errs []error
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink == 0 {
			continue
		}
		rel := path.Join("Cellar", e.Name())
		if _, err := os.Stat(filepath.Join(r.Prefix, rel)); err != nil {
			errs = append(errs, r.Root.Remove(rel))
		}
	}
	return errors.Join(errs...)
}

// configNotices lists the configuration files that uninstalling leaves in etc.
func (r *Remover) configNotices(name string) {
	pkgetc := path.Join("etc", name)
	var paths []string
	if fi, err := r.Root.Lstat(pkgetc); err == nil && fi.IsDir() {
		paths = r.find(pkgetc)
	}
	if len(paths) > 0 {
		abs := make([]string, len(paths))
		for i, p := range paths {
			abs[i] = filepath.Join(r.Prefix, p)
		}
		slices.Sort(abs)
		r.printf(r.Out, "\n")
		r.warn("The following %s configuration files have not been removed!\nIf desired, remove them manually with `rm -rf`:\n  %s",
			name, strings.Join(slices.Compact(abs), "\n  "))
	}
	if r.FormulaNames == nil {
		return
	}
	unversioned, _, _ := strings.Cut(name, "@")
	entries, err := r.Root.ReadDir("etc")
	if err != nil {
		return
	}
	var candidates []fs.DirEntry
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), unversioned) && !slices.Contains(paths, "etc/"+e.Name()) {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 0 {
		return
	}
	names := r.FormulaNames()
	if names == nil {
		return
	}
	var maybe []string
	for _, e := range candidates {
		base := e.Name()
		if !e.IsDir() {
			base = strings.TrimSuffix(base, path.Ext(base))
		}
		if !names[base] {
			maybe = append(maybe, filepath.Join(r.Prefix, "etc", e.Name()))
		}
	}
	if len(maybe) > 0 {
		slices.Sort(maybe)
		r.printf(r.Out, "\n")
		r.warn("The following may be %s configuration files and have not been removed!\nIf desired, remove them manually with `rm -rf`:\n  %s",
			name, strings.Join(maybe, "\n  "))
	}
}

// find lists a directory and everything below it, without following links.
func (r *Remover) find(dir string) []string {
	out := []string{dir}
	entries, err := r.Root.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, e := range entries {
		p := path.Join(dir, e.Name())
		if e.IsDir() {
			out = append(out, r.find(p)...)
		} else {
			out = append(out, p)
		}
	}
	return out
}

// checkDependents refuses when an installed formula that is not being removed
// needs one of the kegs, going by the runtime dependencies in its receipt.
func (r *Remover) checkDependents(kegs []cellar.Keg, named []string) error {
	required, dependents, err := r.dependentsOf(kegs)
	if err != nil || len(required) == 0 {
		return err
	}
	paths := make([]string, len(required))
	for i, k := range required {
		paths[i] = r.kegPath(k)
	}
	return &DependentsError{Kegs: paths, Dependents: dependents, Named: named}
}

// dependentsOf finds which of the kegs are needed by installed formulae or casks
// that are not themselves going, and by whom.
func (r *Remover) dependentsOf(kegs []cellar.Keg) (required []cellar.Keg, dependents []string, err error) {
	p := r.cellar()
	removed := map[string][]cellar.Keg{}
	var optlinked []string
	for _, k := range kegs {
		removed[k.Name] = append(removed[k.Name], k)
		if v, ok := p.OptVersion(k.Name); ok && v == k.Version {
			optlinked = append(optlinked, k.Name)
		}
	}
	for _, name := range r.rackNames() {
		if _, going := removed[name]; going {
			continue
		}
		dep := r.cellar().DefaultKeg(r.rackKegs(name))
		if dep.Receipt == nil {
			continue
		}
		found := false
		for _, d := range dep.Receipt.RuntimeDependencies {
			dn := strings.TrimPrefix(d.FullName, "homebrew/core/")
			if !slices.Contains(optlinked, dn) {
				continue
			}
			k := cellar.Latest(removed[dn])
			if !slices.ContainsFunc(required, func(x cellar.Keg) bool { return x.Path == k.Path }) {
				required = append(required, k)
			}
			found = true
		}
		if found {
			dependents = append(dependents, name)
		}
	}
	casks, err := CaskDependencies(r.Prefix)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot tell whether a cask needs these formulae: %w", err)
	}
	for _, name := range slices.Sorted(maps.Keys(removed)) {
		tokens := casks[name]
		if len(tokens) == 0 {
			continue
		}
		k := cellar.Latest(removed[name])
		if !slices.ContainsFunc(required, func(x cellar.Keg) bool { return x.Path == k.Path }) {
			required = append(required, k)
		}
		for _, t := range tokens {
			if !slices.Contains(dependents, t) {
				dependents = append(dependents, t)
			}
		}
	}
	slices.Sort(dependents)
	return required, dependents, nil
}
