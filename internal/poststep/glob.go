package poststep

import (
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
)

// glob follows Ruby's Pathname.glob for * ? [..] {a,b} and **/: a dot name only
// matches a pattern that has the dot too. A path without wildcards is returned as is.
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
