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

// Match is a formula whose description matched.
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
// suggestions are not included.
func Names(idx *api.Index, query string) ([]string, error) {
	match, err := newMatcher(query)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	canonical := map[string]string{} // alias to formula name
	for _, f := range idx.Formulae() {
		if match(f.Name) {
			names[f.Name] = true
		}
		for _, a := range f.Aliases {
			if match(a) {
				names[a] = true
				canonical[a] = f.Name
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

// Descriptions returns the formulae whose description matches query, by name.
func Descriptions(idx *api.Index, query string) ([]Match, error) {
	match, err := newMatcher(query)
	if err != nil {
		return nil, err
	}
	var out []Match
	for _, f := range idx.Formulae() {
		if f.Desc != "" && match(f.Desc) {
			out = append(out, Match{f.Name, f.Desc})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
