package uninstall

import (
	"errors"
	"fmt"
	"path"
	"slices"

	"github.com/jigsmiths/brw/internal/cellar"
)

// unneeded finds the kegs of formulae that were only installed as dependencies
// and that nothing needs any more, the way brew's Utils::Autoremove does.
// Formulae come out in the order they become unneeded.
func (r *Remover) unneeded() ([]cellar.Keg, error) {
	var rest []cellar.Keg
	for _, name := range r.rackNames() {
		rest = append(rest, r.cellar().DefaultKeg(r.rackKegs(name)))
	}
	var out []cellar.Keg
	for {
		keep := r.namesToKeep(rest)
		var step []cellar.Keg
		for _, k := range rest {
			rec := k.Receipt
			if !keep[k.Name] && rec != nil && rec.OnRequestRecorded() && !rec.InstalledOnRequest {
				step = append(step, k)
			}
		}
		if len(step) == 0 {
			break
		}
		out = append(out, step...)
		rest = slices.DeleteFunc(rest, func(k cellar.Keg) bool { return slices.Contains(step, k) })
	}
	if len(out) == 0 {
		return nil, nil
	}
	casks, err := caskDependencies(r.Prefix)
	if err != nil {
		return nil, fmt.Errorf("cannot tell whether a cask needs these formulae: %w", err)
	}
	out = slices.DeleteFunc(out, func(k cellar.Keg) bool { return len(casks[k.Name]) > 0 })
	return out, nil
}

// namesToKeep is every formula some installed formula needs at run time, plus
// the formulae built from source and what they needed to be built.
func (r *Remover) namesToKeep(kegs []cellar.Keg) map[string]bool {
	keep := map[string]bool{}
	for _, k := range kegs {
		rec := k.Receipt
		if rec != nil {
			for _, d := range rec.RuntimeDependencies {
				keep[stripTap(d.FullName)] = true
			}
			if rec.PouredFromBottle {
				continue
			}
			keep[k.Name] = true
		}
		if r.BuildDeps != nil {
			for _, d := range r.BuildDeps(k.Name) {
				keep[d] = true
			}
		}
	}
	return keep
}

// Unneeded lists the formulae Autoremove would remove, in the order it removes them.
func (r *Remover) Unneeded() ([]string, error) {
	kegs, err := r.unneeded()
	names := make([]string, len(kegs))
	for i, k := range kegs {
		names[i] = k.Name
	}
	return names, err
}

// Autoremove uninstalls the formulae nothing needs any more.
func (r *Remover) Autoremove() error {
	kegs, err := r.unneeded()
	if err != nil || len(kegs) == 0 {
		return err
	}
	names := make([]string, len(kegs))
	for i, k := range kegs {
		names[i] = k.Name
	}
	verb := pick(r.DryRun, "Would autoremove", "Autoremoving")
	noun := pick(len(names) == 1, "formula", "formulae")
	r.printf(r.Out, "==> %s %d unneeded %s:\n", verb, len(names), noun)
	for _, n := range slices.Sorted(slices.Values(names)) {
		r.printf(r.Out, "%s\n", n)
	}
	if r.DryRun {
		return nil
	}
	var failed []error
	for _, k := range kegs {
		if err := r.removeRack(rack{k.Name, []cellar.Keg{k}}, Options{}); err != nil {
			failed = append(failed, err)
		}
	}
	failed = append(failed, r.dropDanglingRacks())
	return errors.Join(failed...)
}

func stripTap(fullName string) string { return path.Base(fullName) }
