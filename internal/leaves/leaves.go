// Package leaves lists the installed formulae that nothing else needs.
package leaves

import (
	"path"
	"slices"

	"github.com/jigsmiths/brw/internal/cellar"
)

// Filter narrows the leaves by how they were installed.
type Filter int

const (
	All Filter = iota
	OnRequest
	AsDependency
)

// List returns the full names of the formulae that no other installed formula
// or cask needs at run time, sorted, as brew leaves does. It goes by the
// runtime dependencies in the receipt of each formula's default keg. casks maps
// a formula to the casks that need it.
func List(p cellar.Prefix, kegs []cellar.Keg, casks map[string][]string, filter Filter) []string {
	racks := map[string][]cellar.Keg{}
	for _, k := range kegs {
		racks[k.Name] = append(racks[k.Name], k)
	}
	needed := map[string]bool{}
	for name := range casks {
		needed[name] = true
	}
	defaults := make([]cellar.Keg, 0, len(racks))
	for _, rack := range racks {
		k := p.DefaultKeg(rack)
		defaults = append(defaults, k)
		if k.Receipt == nil {
			continue
		}
		for _, d := range k.Receipt.RuntimeDependencies {
			needed[path.Base(d.FullName)] = true
		}
	}
	var out []string
	for _, k := range defaults {
		onRequest := k.Receipt != nil && k.Receipt.InstalledOnRequest
		switch {
		case needed[k.Name], filter == OnRequest && !onRequest, filter == AsDependency && onRequest:
			continue
		}
		out = append(out, fullName(k))
	}
	slices.Sort(out)
	return out
}

// fullName is the name with its tap, except for homebrew/core.
func fullName(k cellar.Keg) string {
	if k.Receipt == nil || k.Receipt.Source.Tap == "" || k.Receipt.Source.Tap == "homebrew/core" {
		return k.Name
	}
	return k.Receipt.Source.Tap + "/" + k.Name
}
