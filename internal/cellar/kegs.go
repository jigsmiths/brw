package cellar

import "github.com/jigsmiths/brw/internal/version"

// Latest is the keg with the greatest version scheme, then the greatest version.
func Latest(kegs []Keg) Keg {
	best := kegs[0]
	for _, k := range kegs[1:] {
		if newer(k, best) {
			best = k
		}
	}
	return best
}

func newer(a, b Keg) bool {
	scheme := func(k Keg) int {
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

// DefaultKeg is the keg brew means when no version is named: the one the opt
// record names, else the linked one, else the latest. kegs are all of one formula.
func (p Prefix) DefaultKeg(kegs []Keg) Keg {
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
	return Latest(kegs)
}
