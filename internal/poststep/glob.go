package poststep

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// glob expands a spec's path the way Ruby's Pathname.glob does for the patterns
// formulae use: * ? [..] {a,b} and **/. Names that start with a dot only match
// a pattern that does too. A path without wildcards is returned as it is.
func (r *runner) glob(s pathSpec) ([]string, error) {
	abs, err := r.resolve(s)
	if err != nil {
		return nil, err
	}
	if !strings.ContainsAny(abs, "?*[{") {
		return []string{abs}, nil
	}
	root, pattern := splitGlob(abs)
	re, err := globRegexp(pattern)
	if err != nil {
		return nil, err
	}
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if hiddenOnlyByWildcard(rel, pattern) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if re.MatchString(rel) {
			out = append(out, p)
		}
		return nil
	})
	return out, nil
}

// splitGlob returns the directory before the first wildcard and the rest.
func splitGlob(abs string) (root, pattern string) {
	parts := strings.Split(abs, string(filepath.Separator))
	for i, part := range parts {
		if strings.ContainsAny(part, "?*[{") {
			root = strings.Join(parts[:i], string(filepath.Separator))
			if root == "" {
				root = string(filepath.Separator)
			}
			return root, strings.Join(parts[i:], "/")
		}
	}
	return abs, ""
}

// hiddenOnlyByWildcard reports whether some component of rel starts with a dot
// where the pattern has a wildcard that does not.
func hiddenOnlyByWildcard(rel, pattern string) bool {
	pats := strings.Split(pattern, "/")
	for i, comp := range strings.Split(rel, "/") {
		if !strings.HasPrefix(comp, ".") {
			continue
		}
		if i < len(pats) && strings.HasPrefix(pats[i], ".") {
			continue
		}
		return true
	}
	return false
}

func globRegexp(pattern string) (*regexp.Regexp, error) {
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pattern); i++ {
		switch c := pattern[i]; c {
		case '*':
			switch {
			case strings.HasPrefix(pattern[i:], "**/"):
				b.WriteString("(?:.*/)?")
				i += 2
			case strings.HasPrefix(pattern[i:], "**"):
				b.WriteString(".*")
				i++
			default:
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := strings.IndexByte(pattern[i:], ']')
			if end < 0 {
				b.WriteString(`\[`)
				continue
			}
			b.WriteString(pattern[i : i+end+1])
			i += end
		case '{':
			end := strings.IndexByte(pattern[i:], '}')
			if end < 0 {
				b.WriteString(`\{`)
				continue
			}
			alts := strings.Split(pattern[i+1:i+end], ",")
			for j, a := range alts {
				alts[j] = regexp.QuoteMeta(a)
			}
			b.WriteString("(?:" + strings.Join(alts, "|") + ")")
			i += end
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
