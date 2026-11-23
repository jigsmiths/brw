// Package search finds formulae by name or description.
package search

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/version"
)

type Match struct {
	Name string
	Desc string
}

// newMatcher treats "/re/" as a regular expression, anything else as text.
// Text ignores case and every character except letters, digits, "@" and "+".
func newMatcher(query string) (func(string) bool, error) {
	if len(query) >= 2 && strings.HasPrefix(query, "/") && strings.HasSuffix(query, "/") {
		re, err := regexp.Compile(query[1 : len(query)-1])
		if err != nil {
			return nil, fmt.Errorf("%s is not a valid regex: %w", query, err)
		}
		return re.MatchString, nil
	}
	want := simplify(query)
	return func(s string) bool { return strings.Contains(simplify(s), want) }, nil
}

func simplify(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '@', r == '+':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		}
		return -1
	}, s)
}

// Names returns the sorted names and aliases matching query. An alias is
// dropped when the formula it points at matches too. Brew's fuzzy spelling
// suggestions are not included. Formulae shown rejects are left out; nil keeps all.
func Names(idx *api.Index, query string, shown func(*api.Formula) bool) ([]string, error) {
	match, err := newMatcher(query)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	canonical := map[string]string{} // alias to formula name
	for _, f := range idx.Formulae() {
		if shown != nil && !shown(f) {
			continue
		}
		full, tapPrefix := f.Name, ""
		if f.Tap != "" && f.Tap != "homebrew/core" {
			tapPrefix = f.Tap + "/"
			full = tapPrefix + f.Name
		}
		if match(full) {
			names[full] = true
		}
		for _, a := range f.Aliases {
			if a = tapPrefix + a; match(a) {
				names[a] = true
				canonical[a] = full
			}
		}
	}
	var out []string
	for n := range names {
		if c, isAlias := canonical[n]; isAlias && names[c] {
			continue
		}
		out = append(out, n)
	}
	sortNames(out)
	return out, nil
}

// sortNames orders names as brew does: by the name before any "@", then by the
// version after it, with an unversioned name first.
func sortNames(names []string) {
	split := func(name string) (string, version.Version) {
		base, v, _ := strings.Cut(name, "@")
		if v == "" {
			v = "0"
		}
		parsed, _ := version.Parse(v)
		return base, parsed
	}
	slices.SortStableFunc(names, func(a, b string) int {
		baseA, verA := split(a)
		baseB, verB := split(b)
		return cmp.Or(strings.Compare(baseA, baseB), verA.Compare(verB), strings.Compare(a, b))
	})
}

// CaskTokens returns the sorted tokens matching query. As with Names, brew's
// fuzzy spelling suggestions are not included.
func CaskTokens(idx *api.CaskIndex, query string) ([]string, error) {
	match, err := newMatcher(query)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range idx.Casks() {
		if match(c.Token) {
			out = append(out, c.Token)
		}
	}
	slices.Sort(out)
	return out, nil
}

type CaskMatch struct {
	Token string
	Names string // the cask's names, joined by ", "
	Desc  string
}

// CaskDescriptions returns the casks whose names or description match query,
// by token. Like brew, it leaves out casks without a description.
func CaskDescriptions(idx *api.CaskIndex, query string) ([]CaskMatch, error) {
	match, err := newMatcher(query)
	if err != nil {
		return nil, err
	}
	var out []CaskMatch
	for _, c := range idx.Casks() {
		names := strings.Join(c.Names, ", ")
		if c.Desc != "" && (match(c.Desc) || (names != "" && match(names))) {
			out = append(out, CaskMatch{c.Token, names, c.Desc})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Token < out[j].Token })
	return out, nil
}

func Descriptions(idx *api.Index, query string) ([]Match, error) {
	match, err := newMatcher(query)
	if err != nil {
		return nil, err
	}
	var out []Match
	for _, f := range idx.Formulae() {
		if f.Desc != "" && match(f.Desc) {
			name := f.Name
			if f.Tap != "" && f.Tap != "homebrew/core" {
				name = f.Tap + "/" + f.Name
			}
			out = append(out, Match{name, f.Desc})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
