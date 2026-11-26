package poststep

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
)

const (
	rubyIgnoreCase = 1
	rubyExtended   = 2
	rubyMultiline  = 4
)

type inreplaceError map[string][]string

func (e inreplaceError) Error() string {
	var b strings.Builder
	b.WriteString("inreplace failed\n")
	for path, errs := range e {
		b.WriteString(path + ":\n")
		for _, m := range errs {
			b.WriteString("  " + m + "\n")
		}
	}
	return b.String()
}

func (r *runner) inreplace(s step) error {
	abs, err := r.target(s.Path)
	if err != nil {
		return err
	}
	before, after := r.expand(s.Before), r.expand(s.After)
	b, err := os.ReadFile(abs)
	if err != nil {
		return err
	}
	re, shown, err := compilePattern(before, s.Regexp, s.RegexpOptions)
	if err != nil {
		return err
	}
	out, changed := substitute(string(b), re, after, !s.FirstOnly)
	if !changed && !s.SkipAudit {
		return inreplaceError{abs: {fmt.Sprintf("expected replacement of %s with %s", shown, rubyInspect(after))}}
	}
	mode := fileMode
	if fi, err := os.Stat(abs); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := r.removeAny(abs); err != nil {
		return err
	}
	return r.writeFile(abs, []byte(out), mode)
}

func compilePattern(before string, isRegexp bool, options int) (*regexp.Regexp, string, error) {
	if !isRegexp {
		return regexp.MustCompile(regexp.QuoteMeta(before)), rubyInspect(before), nil
	}
	src, err := translateRegexp(before, options)
	if err != nil {
		return nil, "", err
	}
	re, err := regexp.Compile(src)
	if err != nil {
		return nil, "", fmt.Errorf("unsupported regexp /%s/: %w", before, err)
	}
	var flags strings.Builder
	for _, f := range []struct {
		bit  int
		name string
	}{{rubyMultiline, "m"}, {rubyIgnoreCase, "i"}, {rubyExtended, "x"}} {
		if options&f.bit != 0 {
			flags.WriteString(f.name)
		}
	}
	return re, "/" + before + "/" + flags.String(), nil
}

var errRegexpFeature = errors.New("not supported by this regular expression engine")

// Lookarounds, backreferences and atomic or possessive groups have no RE2 form and are refused.
func translateRegexp(src string, options int) (string, error) {
	var b strings.Builder
	b.WriteString("(?m")
	if options&rubyIgnoreCase != 0 {
		b.WriteString("i")
	}
	if options&rubyMultiline != 0 {
		b.WriteString("s")
	}
	b.WriteString(")")
	inClass := false
	for i := 0; i < len(src); i++ {
		c := src[i]
		switch {
		case c == '\\' && i+1 < len(src):
			i++
			switch n := src[i]; {
			case n == 'h':
				b.WriteString(map[bool]string{true: `0-9a-fA-F`, false: `[0-9a-fA-F]`}[inClass])
			case n == 'H' && !inClass:
				b.WriteString(`[^0-9a-fA-F]`)
			case n == 'Z' && !inClass:
				b.WriteString(`(?:\n?\z)`)
			case n >= '1' && n <= '9' || n == 'k' || n == 'G' || n == 'R' || n == 'X':
				return "", fmt.Errorf("unsupported regexp /%s/: \\%c is %w", src, n, errRegexpFeature)
			default:
				b.WriteByte('\\')
				b.WriteByte(n)
			}
		case inClass:
			if c == ']' {
				inClass = false
			}
			b.WriteByte(c)
		case c == '[':
			inClass = true
			b.WriteByte(c)
			if i+1 < len(src) && src[i+1] == '^' {
				i++
				b.WriteByte('^')
			}
			if i+1 < len(src) && src[i+1] == ']' {
				i++
				b.WriteString(`\]`)
			}
		case c == '(' && strings.HasPrefix(src[i:], "(?"):
			rest := src[i+2:]
			switch {
			case strings.HasPrefix(rest, "=") || strings.HasPrefix(rest, "!") || strings.HasPrefix(rest, "<=") ||
				strings.HasPrefix(rest, "<!") || strings.HasPrefix(rest, ">"):
				return "", fmt.Errorf("unsupported regexp /%s/: a lookaround or atomic group is %w", src, errRegexpFeature)
			case strings.HasPrefix(rest, "<") || strings.HasPrefix(rest, "'"):
				b.WriteString("(?P<")
				i += 2
				if src[i] == '\'' {
					end := strings.IndexByte(src[i+1:], '\'')
					if end < 0 {
						return "", fmt.Errorf("unsupported regexp /%s/", src)
					}
					b.WriteString(src[i+1:i+1+end] + ">")
					i += end + 1
				}
			default:
				b.WriteString("(?")
				i++
			}
		case options&rubyExtended != 0 && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
		case options&rubyExtended != 0 && c == '#':
			for i+1 < len(src) && src[i+1] != '\n' {
				i++
			}
		case (c == '*' || c == '+' || c == '?' || c == '}') && i+1 < len(src) && src[i+1] == '+':
			return "", fmt.Errorf("unsupported regexp /%s/: a possessive quantifier is %w", src, errRegexpFeature)
		default:
			b.WriteByte(c)
		}
	}
	return b.String(), nil
}

func substitute(text string, re *regexp.Regexp, repl string, global bool) (string, bool) {
	matches := re.FindAllStringSubmatchIndex(text, -1)
	if !global && len(matches) > 1 {
		matches = matches[:1]
	}
	if len(matches) == 0 {
		return text, false
	}
	var b strings.Builder
	last := 0
	for _, m := range matches {
		b.WriteString(text[last:m[0]])
		b.WriteString(expandReplacement(repl, text, m, re))
		last = m[1]
	}
	b.WriteString(text[last:])
	return b.String(), true
}

func expandReplacement(repl, text string, m []int, re *regexp.Regexp) string {
	group := func(n int) string {
		if n*2+1 < len(m) && m[n*2] >= 0 {
			return text[m[n*2]:m[n*2+1]]
		}
		return ""
	}
	var b strings.Builder
	for i := 0; i < len(repl); i++ {
		if repl[i] != '\\' || i+1 == len(repl) {
			b.WriteByte(repl[i])
			continue
		}
		i++
		switch n := repl[i]; {
		case n >= '0' && n <= '9':
			b.WriteString(group(int(n - '0')))
		case n == '&':
			b.WriteString(group(0))
		case n == '`':
			b.WriteString(text[:m[0]])
		case n == '\'':
			b.WriteString(text[m[1]:])
		case n == '\\':
			b.WriteByte('\\')
		case n == 'k' && i+1 < len(repl) && repl[i+1] == '<':
			end := strings.IndexByte(repl[i:], '>')
			if end < 0 {
				b.WriteString(`\k`)
				continue
			}
			name := repl[i+2 : i+end]
			if idx := re.SubexpIndex(name); idx >= 0 {
				b.WriteString(group(idx))
			} else if n, err := strconv.Atoi(name); err == nil {
				b.WriteString(group(n))
			}
			i += end
		default:
			b.WriteByte('\\')
			b.WriteByte(n)
		}
	}
	return b.String()
}

func rubyInspect(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i, r := range s {
		switch r {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case '\n':
			b.WriteString(`\n`)
		case '\t':
			b.WriteString(`\t`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1b:
			b.WriteString(`\e`)
		case '#':
			if i+1 < len(s) && strings.ContainsRune("{$@", rune(s[i+1])) {
				b.WriteByte('\\')
			}
			b.WriteByte('#')
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\x%02X`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
