package version

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// parser is one of brew's Version::Parser rules: a URL rule matches the
// whole spec, a stem rule the file name without its extension.
type parser struct {
	re   *regexp.Regexp
	stem bool
	fix  func(string) string
}

const (
	numericOptionalDots = `(?:\d+(?:\.\d+)*)`
	numericDots         = `(?:\d+(?:\.\d+)+)`
	minorOrPatch        = `(?:\d+(?:\.\d+){1,2})`
	contentSuffix       = `(?:[._-](?i:bin|dist|stable|src|sources?|final|full))`
	prereleaseSuffix    = `(?:[._-]?(?i:alpha|beta|pre|rc)\.?\d{0,2})`
)

func stemP(re string) parser { return parser{re: regexp.MustCompile(re), stem: true} }
func urlP(re string) parser  { return parser{re: regexp.MustCompile(re)} }

// parsers are VERSION_PARSERS from brew's version.rb, in order.
var parsers = []parser{
	stemP(`(?:^|[._-]?)v?(\d{4}-\d{2}-\d{2})`),
	urlP(`github\.com/.+/(?:zip|tar)ball/(?:v|\w+-)?((?:\d+[._-])+\d*)$`),
	urlP(`github\.com/.+/releases/download/(?:[rvV]_?)?(` + numericDots + `)/`),
	urlP(`[_-]([Rr]\d+[AaBb]\d*(?:-\d+)?)`),
	{re: regexp.MustCompile(`((?:\d+_)+\d+)$`), stem: true, fix: func(s string) string { return strings.ReplaceAll(s, "_", ".") }},
	stemP(`[_-](` + numericDots + `-(?:p|P|rc|RC)?\d+)` + contentSuffix + `?$`),
	stemP(`^v?(` + numericDots + `(?:-` + numericOptionalDots + `)+)`),
	urlP(`[-v](` + numericOptionalDots + `)$`),
	stemP(`-(\d+-\d+)`),
	stemP(`-(` + numericOptionalDots + `)$`),
	stemP(`-(` + numericOptionalDots + `(?:.post\d+)?)$`),
	stemP(`-(` + numericOptionalDots + `(?:[abc]|rc|RC)\d*)$`),
	stemP(`-(` + numericOptionalDots + `-(?:alpha|beta|rc)\d*)$`),
	stemP(`-(` + minorOrPatch + `)-w(?:in)?(?:32|64)$`),
	stemP(`\.(` + minorOrPatch + `)\+opam$`),
	stemP(`[_-](` + minorOrPatch + `(?:-\d+)?)[._-](?:i[36]86|x86|x64(?:[_-](?:32|64))?)$`),
	stemP(`[-.vV]?(` + numericDots + prereleaseSuffix + `)`),
	stemP(`(` + numericOptionalDots + `)$`),
	stemP(`[-vV](` + numericDots + `[abc]?)` + contentSuffix + `$`),
	stemP(`-(` + numericDots + `)-`),
	stemP(`_(` + numericOptionalDots + `[abc]?)\.orig$`),
	stemP(`-v?(\d[^-]+)`),
	stemP(`_v?(\d[^_]+)`),
	urlP(`/(?:[rvV]_?)?(\d+\.\d+(?:\.\d+){0,2})`),
	stemP(`\.v(\d+[a-z]?)`),
	urlP(`[-.vV]?(` + numericDots + prereleaseSuffix + `?)`),
}

var (
	sourceforgeDownload = regexp.MustCompile(`(?:sourceforge\.net|sf\.net)/.*/download$`)
	noFileExtension     = regexp.MustCompile(`\.[^a-zA-Z]+$`)
	bottleExt           = regexp.MustCompile(`\.([a-z0-9_]+)\.bottle\.(?:(\d+)\.)?tar\.gz$`)
	archiveExt          = regexp.MustCompile(`(\.(?:tar|cpio|pax)\.(?:gz|bz2|lz|xz|zst|Z))$`)
	versionTail         = regexp.MustCompile(`\b\d+\.\d+[^.]*$`)
)

// Detect is brew's Version.detect: the version in a download's tag, if it
// has one, else in its URL. It returns "" when no rule matches.
func Detect(rawURL, tag string) string {
	spec := rawURL
	if tag != "" {
		spec = tag
	}
	if s, err := url.QueryUnescape(spec); err == nil {
		spec = s
	}
	for _, p := range parsers {
		s := spec
		if p.stem {
			s = stemOf(spec)
		}
		m := p.re.FindStringSubmatch(s)
		if m == nil || m[1] == "" {
			continue
		}
		if p.fix != nil {
			return p.fix(m[1])
		}
		return m[1]
	}
	return ""
}

// stemOf is StemParser.process_spec.
func stemOf(spec string) string {
	if sourceforgeDownload.MatchString(spec) {
		return stem(path.Base(path.Dir(spec)))
	}
	if noFileExtension.MatchString(spec) {
		return path.Base(spec)
	}
	return stem(path.Base(spec))
}

// stem is brew's Pathname#stem: the base name without Pathname#extname.
func stem(base string) string {
	return strings.TrimSuffix(base, extname(base))
}

func extname(base string) string {
	if m := bottleExt.FindString(base); m != "" {
		return m
	}
	if m := archiveExt.FindStringSubmatch(base); m != nil {
		return m[1]
	}
	if versionTail.MatchString(base) && !strings.HasSuffix(base, ".7z") {
		return ""
	}
	if strings.HasPrefix(base, ".") && strings.Count(base, ".") == 1 {
		return ""
	}
	return path.Ext(base)
}
