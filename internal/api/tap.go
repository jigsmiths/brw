package api

import (
	"encoding/json"
	"slices"
	"strings"
	"sync"
)

// TapLoader evaluates a tap's formula called name, returning it and its hash.
type TapLoader func(name string) (Formula, json.RawMessage, error)

// tapFormula is a formula a tap lists, evaluated on first lookup.
type tapFormula struct {
	tap, name string
	load      TapLoader
	once      sync.Once
	f         *Formula
	raw       json.RawMessage
	err       error
}

func (e *tapFormula) get() (*Formula, bool) {
	e.once.Do(func() {
		f, raw, err := e.load(e.name)
		if err == nil {
			e.f, e.raw = &f, raw
		}
		e.err = err
	})
	return e.f, e.err == nil
}

func (e *tapFormula) fullName() string { return e.tap + "/" + e.name }

// coreFormulaTap is the tap of the API's formulae.
const coreFormulaTap = "homebrew/core"

// FromTap says whether the formula is a tap's rather than the API's.
func (f *Formula) FromTap() bool { return f.Tap != "" && f.Tap != coreFormulaTap }

// AddTap adds the formulae a tap lists, found by tap/name and, when neither
// the API nor another tap has it, by name. alt maps the tap's aliases and old
// names to its formula names.
func (i *Index) AddTap(tap string, names []string, alt map[string]string, load TapLoader) {
	if i.tapFull == nil {
		i.tapFull, i.tapBare = map[string]*tapFormula{}, map[string][]*tapFormula{}
	}
	byName := map[string]*tapFormula{}
	for _, n := range names {
		e := &tapFormula{tap: tap, name: n, load: load}
		byName[n] = e
		i.tapOrder = append(i.tapOrder, e)
		i.tapFull[tap+"/"+n] = e
		i.tapBare[n] = append(i.tapBare[n], e)
	}
	for a, n := range alt {
		e, ok := byName[n]
		if !ok {
			continue
		}
		if _, taken := i.tapFull[tap+"/"+a]; !taken {
			i.tapFull[tap+"/"+a] = e
		}
		if !slices.Contains(i.tapBare[a], e) {
			i.tapBare[a] = append(i.tapBare[a], e)
		}
	}
}

// SkipTap records an untrusted tap; onList is told of it whenever every
// formula is listed, as brew warns only then.
func (i *Index) SkipTap(tap string, onList func(tap string)) {
	i.skipped = append(i.skipped, tap)
	i.onSkip = onList
}

func (i *Index) listingTaps() {
	for _, t := range i.skipped {
		i.onSkip(t)
	}
}

// LookupFrom finds a formula in the given tap, the API's when tap is "" or
// homebrew/core.
func (i *Index) LookupFrom(tap, name string) (*Formula, bool) {
	if tap == "" || tap == coreFormulaTap {
		f, ok := i.byName[name]
		return f, ok
	}
	e, ok := i.tapFull[strings.ToLower(tap)+"/"+name]
	if !ok {
		return nil, false
	}
	return e.get()
}

// lookupTap finds a bare name the API does not have in the one tap that has
// it. A name in several taps is ambiguous, as brew says.
func (i *Index) lookupTap(name string) (*Formula, bool) {
	es := i.tapBare[name]
	if len(es) != 1 {
		return nil, false
	}
	return es[0].get()
}

// TapsWith lists the full names of the tap formulae a bare name could mean.
func (i *Index) TapsWith(name string) []string {
	var out []string
	for _, e := range i.tapBare[name] {
		out = append(out, e.fullName())
	}
	slices.Sort(out)
	return out
}

// LoadError says why a tap formula, by full name, could not be evaluated.
func (i *Index) LoadError(fullName string) error {
	e, ok := i.tapFull[fullName]
	if !ok {
		return nil
	}
	e.get()
	return e.err
}

// TapRaw is a tap formula's hash, by full name; core formulae have none.
func (i *Index) TapRaw(fullName string) (json.RawMessage, bool) {
	e, ok := i.tapFull[fullName]
	if !ok {
		return nil, false
	}
	if _, ok := e.get(); !ok {
		return nil, false
	}
	return e.raw, true
}

// tapFormulae evaluates every tap formula, leaving out those that fail.
func (i *Index) tapFormulae() []*Formula {
	i.listingTaps()
	var out []*Formula
	for _, e := range i.tapOrder {
		if f, ok := e.get(); ok {
			out = append(out, f)
		}
	}
	return out
}
