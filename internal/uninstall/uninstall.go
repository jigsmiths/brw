// Package uninstall removes kegs the way brew uninstall does.
package uninstall

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/version"
)

// NoSuchKegError is a formula that is known but has nothing installed.
type NoSuchKegError struct{ Path string }

func (e *NoSuchKegError) Error() string { return "no such keg: " + e.Path }

// UnavailableError is a name that is neither installed nor a formula.
type UnavailableError struct{ Name string }

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("no available formula with the name %q", e.Name)
}

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

// defaultKeg is the keg brew uninstalls when no version is forced: the one the
// opt record names, else the linked one, else the only one, else the latest.
func (r *Remover) defaultKeg(kegs []cellar.Keg) cellar.Keg {
	p := r.cellar()
	name := kegs[0].Name
	if v, ok := p.OptVersion(name); ok {
		for _, k := range kegs {
			if k.Version == v {
				return k
			}
		}
	}
	for _, k := range kegs {
		if p.Linked(name, k.Version) {
			return k
		}
	}
	return latest(kegs)
}

// latest is the keg with the greatest scheme and version.
func latest(kegs []cellar.Keg) cellar.Keg {
	best := kegs[0]
	for _, k := range kegs[1:] {
		if newer(k, best) {
			best = k
		}
	}
	return best
}

func newer(a, b cellar.Keg) bool {
	scheme := func(k cellar.Keg) int {
		if k.Receipt == nil {
			return 0
		}
		return k.Receipt.VersionScheme()
	}
	if scheme(a) != scheme(b) {
		return scheme(a) > scheme(b)
	}
	va, ea := version.ParsePkg(a.Version)
	vb, eb := version.ParsePkg(b.Version)
	if ea != nil || eb != nil {
		return eb != nil && ea == nil
	}
	return va.Compare(vb) > 0
}

// resolve finds the rack a name stands for.
func (r *Remover) resolve(name string) (string, error) {
	name = strings.TrimPrefix(strings.ToLower(name), "homebrew/core/")
	if len(r.rackKegs(name)) > 0 {
		return name, nil
	}
	canonical, known := name, false
	if r.Resolve != nil {
		canonical, known = r.Resolve(name)
		if known && len(r.rackKegs(canonical)) > 0 {
			return canonical, nil
		}
	}
	if known {
		return "", &NoSuchKegError{Path: filepath.Join(r.Prefix, "Cellar", canonical)}
	}
	if r.Resolve == nil {
		return "", &NoSuchKegError{Path: filepath.Join(r.Prefix, "Cellar", name)}
	}
	return "", &UnavailableError{Name: name}
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
			kegs = []cellar.Keg{r.defaultKeg(kegs)}
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
	names := r.FormulaNames()
	entries, err := r.Root.ReadDir("etc")
	if err != nil {
		return
	}
	var maybe []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), unversioned) {
			continue
		}
		base := e.Name()
		if !e.IsDir() {
			base = strings.TrimSuffix(base, path.Ext(base))
		}
		abs := filepath.Join(r.Prefix, "etc", e.Name())
		if names[base] || slices.Contains(paths, "etc/"+e.Name()) {
			continue
		}
		maybe = append(maybe, abs)
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
	p := r.cellar()
	removed := map[string][]cellar.Keg{}
	var optlinked []string
	for _, k := range kegs {
		removed[k.Name] = append(removed[k.Name], k)
		if v, ok := p.OptVersion(k.Name); ok && v == k.Version {
			optlinked = append(optlinked, k.Name)
		}
	}
	var required []cellar.Keg
	var dependents []string
	for _, name := range r.rackNames() {
		if _, going := removed[name]; going {
			continue
		}
		dep := r.defaultKeg(r.rackKegs(name))
		if dep.Receipt == nil {
			continue
		}
		found := false
		for _, d := range dep.Receipt.RuntimeDependencies {
			dn := strings.TrimPrefix(d.FullName, "homebrew/core/")
			if !slices.Contains(optlinked, dn) {
				continue
			}
			k := latest(removed[dn])
			if !slices.ContainsFunc(required, func(x cellar.Keg) bool { return x.Path == k.Path }) {
				required = append(required, k)
			}
			found = true
		}
		if found {
			dependents = append(dependents, name)
		}
	}
	if len(required) == 0 {
		return nil
	}
	slices.Sort(dependents)
	paths := make([]string, len(required))
	for i, k := range required {
		paths[i] = r.kegPath(k)
	}
	return &DependentsError{Kegs: paths, Dependents: dependents, Named: named}
}
