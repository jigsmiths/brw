// Package version orders Homebrew versions the way brew does.
package version

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type kind int

const (
	null kind = iota
	numeric
	str
	alpha
	beta
	pre
	rc
	patch
	post
)

type token struct {
	kind kind
	val  string // text for string-like tokens, digits without leading zeros for numeric
}

var (
	scanPattern = regexp.MustCompile(`(?i)alpha[0-9]*|a[0-9]+|beta[0-9]*|b[0-9]+|pre[0-9]*|rc[0-9]*|p[0-9]*|.post[0-9]+|[0-9]+|[a-z]+`)
	digits      = regexp.MustCompile(`[0-9]+`)
	// Order matters: it is the order brew tries them in.
	classes = []struct {
		kind kind
		re   *regexp.Regexp
	}{
		{alpha, regexp.MustCompile(`(?i)^(?:alpha[0-9]*|a[0-9]+)$`)},
		{beta, regexp.MustCompile(`(?i)^(?:beta[0-9]*|b[0-9]+)$`)},
		{rc, regexp.MustCompile(`(?i)^(?:rc[0-9]*)$`)},
		{pre, regexp.MustCompile(`(?i)^(?:pre[0-9]*)$`)},
		{patch, regexp.MustCompile(`(?i)^(?:p[0-9]*)$`)},
		{post, regexp.MustCompile(`(?i)^(?:.post[0-9]+)$`)},
		{numeric, regexp.MustCompile(`^[0-9]+$`)},
	}
	head = regexp.MustCompile(`^HEAD(?:-.*)?$`)
)

func newToken(s string) token {
	for _, c := range classes {
		if c.re.MatchString(s) {
			if c.kind == numeric {
				return token{numeric, trimZeros(s)}
			}
			return token{c.kind, s}
		}
	}
	return token{str, s}
}

func trimZeros(s string) string { return strings.TrimLeft(s, "0") }

// cmpNum compares non-negative integers given as digit strings.
func cmpNum(a, b string) int {
	a, b = trimZeros(a), trimZeros(b)
	if len(a) != len(b) {
		return sign(len(a) - len(b))
	}
	return strings.Compare(a, b)
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

// rev is the number inside a composite token such as "rc2".
func (t token) rev() string { return digits.FindString(t.val) }

func (t token) stringLike() bool { return t.kind >= str }

func (t token) isZero() bool { return t.val == "" }

// cmpToken mirrors the Token#<=> methods in Homebrew's version.rb.
func cmpToken(a, b token) int {
	switch a.kind {
	case null:
		switch b.kind {
		case null:
			return 0
		case numeric:
			if b.isZero() {
				return 0
			}
			return -1
		case alpha, beta, pre, rc:
			return 1
		}
		return -1
	case numeric:
		switch b.kind {
		case numeric:
			return cmpNum(a.val, b.val)
		case null:
			if a.isZero() {
				return 0
			}
		}
		return 1
	case str:
		return cmpString(a, b)
	case alpha:
		return cmpComposite(a, b, map[kind]int{alpha: 0, beta: -1, rc: -1, pre: -1, patch: -1, post: -1})
	case beta:
		return cmpComposite(a, b, map[kind]int{beta: 0, alpha: 1, pre: -1, rc: -1, patch: -1, post: -1})
	case pre:
		return cmpComposite(a, b, map[kind]int{pre: 0, alpha: 1, beta: 1, rc: -1, patch: -1, post: -1})
	case rc:
		return cmpComposite(a, b, map[kind]int{rc: 0, alpha: 1, beta: 1, pre: 1, patch: -1, post: -1})
	case patch:
		return cmpComposite(a, b, map[kind]int{patch: 0, alpha: 1, beta: 1, rc: 1, pre: 1})
	case post:
		return cmpComposite(a, b, map[kind]int{post: 0, alpha: 1, beta: 1, rc: 1, pre: 1})
	}
	return 0
}

// cmpComposite compares a release-stage token with b. order gives the result
// per kind of b (0 means compare their numbers); other kinds use the string rules.
func cmpComposite(a, b token, order map[kind]int) int {
	if c, ok := order[b.kind]; ok {
		if c == 0 {
			return cmpNum(a.rev(), b.rev())
		}
		return c
	}
	return cmpString(a, b)
}

// cmpString is StringToken#<=>, which the other string-like tokens inherit.
func cmpString(a, b token) int {
	switch {
	case b.stringLike():
		return sign(strings.Compare(a.val, b.val))
	case b.kind == numeric:
		return -1
	}
	return -cmpToken(b, a)
}

// Version is a Homebrew version such as "1.2.3" or "2.0rc1".
type Version struct {
	s      string
	tokens []token
}

func Parse(s string) (Version, error) {
	if strings.TrimSpace(s) == "" {
		return Version{}, errors.New("version must not be empty")
	}
	v := Version{s: s}
	for _, m := range scanPattern.FindAllString(s, -1) {
		v.tokens = append(v.tokens, newToken(m))
	}
	return v, nil
}

func (v Version) String() string { return v.s }

func (v Version) IsHead() bool { return head.MatchString(v.s) }

func (v Version) Compare(o Version) int {
	if v.s == o.s {
		return 0
	}
	vh, oh := v.IsHead(), o.IsHead()
	switch {
	case vh && oh:
		return 0
	case vh:
		return 1
	case oh:
		return -1
	}

	nullTok := token{kind: null}
	at := func(ts []token, i int) token {
		if i < len(ts) {
			return ts[i]
		}
		return nullTok
	}
	n := max(len(v.tokens), len(o.tokens))
	l, r := 0, 0
	for l < n {
		a, b := at(v.tokens, l), at(o.tokens, r)
		switch {
		case cmpToken(a, b) == 0:
			l++
			r++
		case a.kind == numeric && b.kind != numeric:
			if cmpToken(a, nullTok) > 0 {
				return 1
			}
			l++
		case a.kind != numeric && b.kind == numeric:
			if cmpToken(b, nullTok) > 0 {
				return -1
			}
			r++
		default:
			return cmpToken(a, b)
		}
	}
	return 0
}

// PkgVersion is a version plus a formula revision, written "1.2_3".
type PkgVersion struct {
	Version  Version
	Revision int
}

var pkgPattern = regexp.MustCompile(`^(.+?)(?:_([0-9]+))?$`)

func NewPkg(version string, revision int) (PkgVersion, error) {
	v, err := Parse(version)
	return PkgVersion{v, revision}, err
}

func ParsePkg(s string) (PkgVersion, error) {
	m := pkgPattern.FindStringSubmatch(s)
	if m == nil {
		return PkgVersion{}, fmt.Errorf("invalid version %q", s)
	}
	revision := 0
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil {
			return PkgVersion{}, fmt.Errorf("invalid revision in %q: %w", s, err)
		}
		revision = n
	}
	return NewPkg(m[1], revision)
}

func (p PkgVersion) String() string {
	if p.Revision > 0 {
		return fmt.Sprintf("%s_%d", p.Version, p.Revision)
	}
	return p.Version.String()
}

func (p PkgVersion) Compare(o PkgVersion) int {
	if c := p.Version.Compare(o.Version); c != 0 {
		return c
	}
	return sign(p.Revision - o.Revision)
}
