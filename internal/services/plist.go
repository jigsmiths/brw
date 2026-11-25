package services

import (
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const userNameKeyName = "UserName"

// ApplyUserName sets UserName and re-serializes the plist as the Ruby plist gem's to_plist does.
func ApplyUserName(plist []byte, user string) ([]byte, error) {
	root, err := parsePlist(string(plist))
	if err != nil {
		return nil, err
	}
	dict, ok := root.(*plistDict)
	if !ok {
		return nil, errors.New("the plist is not a dictionary")
	}
	if err := checkPrintable(dict); err != nil {
		return nil, err
	}
	dict.set("s:"+userNameKeyName, userNameKeyName, userNameKeyName, user)
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n")
	b.WriteString("<!DOCTYPE plist PUBLIC \"-//Apple//DTD PLIST 1.0//EN\" \"http://www.apple.com/DTDs/PropertyList-1.0.dtd\">\n")
	b.WriteString("<plist version=\"1.0\">\n")
	emitPlist(&b, dict, 0)
	b.WriteString("</plist>\n")
	return []byte(b.String()), nil
}

type plistDate string

type plistData []byte

// plistNil is Ruby's nil, which an empty <plist/> yields.
type plistNil struct{}

// plistDict is a Ruby Hash: keys are compared by type and value, and keep insertion order.
type plistDict struct {
	entries []dictEntry
}

// plistUnprintable is a key whose to_s differs on every run or needs a date offset, such as a data or date element.
type plistUnprintable struct{}

func checkPrintable(v any) error {
	switch x := v.(type) {
	case *plistDict:
		for _, e := range x.entries {
			if _, bad := e.key.(plistUnprintable); bad {
				return errors.New("a dictionary key of this type is not supported")
			}
			if err := checkPrintable(e.val); err != nil {
				return err
			}
		}
	case []any:
		for _, e := range x {
			if err := checkPrintable(e); err != nil {
				return err
			}
		}
	}
	return nil
}

type dictEntry struct {
	id   string
	text string
	key  any
	val  any
}

func (d *plistDict) set(id, text string, key, val any) {
	for i := range d.entries {
		if d.entries[i].id == id {
			d.entries[i].val = val
			return
		}
	}
	d.entries = append(d.entries, dictEntry{id, text, key, val})
}

type plistNode struct {
	tag      string
	text     string
	children []*plistNode
}

const (
	plistTags   = "plist|dict|key|string|array|integer|true|false|real|date|data"
	commentOpen = "<!--"
	asciiLimit  = 0x80
)

var (
	plistTagSet = map[string]bool{}
	startTag    = regexp.MustCompile(`(?i)\A<(` + plistTags + `)([^>]*)>`)
	endTag      = regexp.MustCompile(`(?i)\A</(` + plistTags + `)[^>]*>`)
	selfClosing = regexp.MustCompile(`(?m)/$`)
	xmlDecl     = regexp.MustCompile(`(?s)\A<\?xml\s+(.*?)\?>*`)
	xmlEncoding = regexp.MustCompile(`(?:\A|\s)encoding=(?:"([^\n]*?)"|'([^\n]*?)')(?:\s|\z)`)
	doctype     = regexp.MustCompile(`(?s)\A\s*<!DOCTYPE\s+(.*?)(\[|>)`)
	cdata       = regexp.MustCompile(`\A<!\[CDATA\[(.*?)\]\]>`)
	commentEnd  = regexp.MustCompile(`(?s)\A.*?-->`)
	wideEncoded = regexp.MustCompile(`(?i)\A(utf-?(16|32)(le|be)?|ucs-[24](le|be)|utf-7|cp65000)\z`)
)

func init() {
	for t := range strings.SplitSeq(plistTags, "|") {
		plistTagSet[t] = true
	}
}

// declaredEncodingFails reports whether Ruby's force_encoding to the declared encoding breaks the rest of the scan.
func declaredEncodingFails(decl, rest string) bool {
	loc := xmlEncoding.FindStringSubmatchIndex(decl)
	if loc == nil {
		return false
	}
	if loc[4] >= 0 {
		return true
	}
	name := decl[loc[2]:loc[3]]
	if wideEncoded.MatchString(name) {
		return rest != ""
	}
	if strings.EqualFold(name, "us-ascii") || strings.EqualFold(name, "ascii") || strings.EqualFold(name, "ansi_x3.4-1968") {
		for i := range len(rest) {
			if rest[i] >= asciiLimit {
				return true
			}
		}
	}
	return false
}

// parsePlist follows the Ruby plist gem's stream parser with marshal off, including what it rejects.
func parsePlist(src string) (any, error) {
	var open []*plistNode
	var result any
	done := false
	closeTop := func() error {
		if len(open) == 0 {
			return errors.New("unbalanced end tag in the plist")
		}
		last := open[len(open)-1]
		open = open[:len(open)-1]
		if len(open) > 0 {
			open[len(open)-1].children = append(open[len(open)-1].children, last)
			return nil
		}
		value, err := last.value()
		if err != nil {
			return err
		}
		result, done = value, true
		return nil
	}
	for rest := src; rest != ""; {
		if strings.HasPrefix(rest, commentOpen) {
			rest = rest[len(commentOpen):]
			if loc := commentEnd.FindStringIndex(rest); loc != nil {
				rest = rest[loc[1]:]
			}
		} else if m := xmlDecl.FindStringSubmatch(rest); m != nil {
			rest = rest[len(m[0]):]
			if declaredEncodingFails(m[1], rest) {
				return nil, errors.New("the plist declares an encoding the parser cannot scan")
			}
		} else if m := doctype.FindString(rest); m != "" {
			rest = rest[len(m):]
		} else if m := startTag.FindStringSubmatch(rest); m != nil {
			rest = rest[len(m[0]):]
			if !plistTagSet[m[1]] {
				return nil, fmt.Errorf("unimplemented element %s in the plist", m[1])
			}
			open = append(open, &plistNode{tag: m[1]})
			if selfClosing.MatchString(m[2]) {
				if err := closeTop(); err != nil {
					return nil, err
				}
			}
		} else if rest[0] != '<' {
			end := strings.IndexByte(rest, '<')
			if end < 0 {
				end = len(rest)
			}
			if len(open) > 0 {
				open[len(open)-1].text += rest[:end]
			}
			rest = rest[end:]
		} else if m := cdata.FindStringSubmatch(rest); m != nil {
			rest = rest[len(m[0]):]
			if len(open) > 0 {
				open[len(open)-1].text += m[1]
			}
		} else if m := endTag.FindString(rest); m != "" {
			rest = rest[len(m):]
			if err := closeTop(); err != nil {
				return nil, err
			}
		} else {
			return nil, errors.New("unimplemented element in the plist")
		}
	}
	if !done || result == nil {
		return nil, errors.New("the plist has no content")
	}
	return result, nil
}

var (
	numericRef   = regexp.MustCompile(`&#(x[0-9a-fA-F]+|\d+);`)
	htmlUnescape = strings.NewReplacer("&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&apos;", "'", "&#39;", "'")
)

func unescapeHTML(s string) string {
	s = numericRef.ReplaceAllStringFunc(s, func(ref string) string {
		body := ref[2 : len(ref)-1]
		base := 10
		if body[0] == 'x' {
			body, base = body[1:], 16
		}
		n, err := strconv.ParseInt(body, base, 32)
		if err != nil || n < 0 || n > 0x10ffff {
			return ref
		}
		return string(rune(n))
	})
	return htmlUnescape.Replace(s)
}

func isRubySpace(c byte) bool {
	return c == ' ' || (c >= '\t' && c <= '\r')
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// scanDigits reads digits where a single underscore may sit between two digits, as Ruby's to_i and to_f allow.
func scanDigits(s string, i int) (digits string, next int) {
	var b strings.Builder
	for i < len(s) {
		switch {
		case isDigit(s[i]):
			b.WriteByte(s[i])
		case s[i] == '_' && b.Len() > 0 && i+1 < len(s) && isDigit(s[i+1]) && isDigit(s[i-1]):
		default:
			return b.String(), i
		}
		i++
	}
	return b.String(), i
}

func skipSpace(s string) int {
	i := 0
	for i < len(s) && isRubySpace(s[i]) {
		i++
	}
	return i
}

func rubyToI(s string) *big.Int {
	i := skipSpace(s)
	negative := false
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		negative = s[i] == '-'
		i++
	}
	if i+2 < len(s) && s[i] == '0' && (s[i+1] == 'd' || s[i+1] == 'D') && isDigit(s[i+2]) {
		i += 2
	}
	digits, _ := scanDigits(s, i)
	n, _ := new(big.Int).SetString(digits, 10)
	if n == nil {
		return new(big.Int)
	}
	if negative {
		n.Neg(n)
	}
	return n
}

func rubyToF(s string) float64 {
	i := skipSpace(s)
	sign := ""
	if i < len(s) && (s[i] == '+' || s[i] == '-') {
		sign = s[i : i+1]
		i++
	}
	whole, i := scanDigits(s, i)
	frac := ""
	if i+1 < len(s) && s[i] == '.' && isDigit(s[i+1]) {
		frac, i = scanDigits(s, i+1)
	} else if whole != "" && i+1 < len(s) && s[i] == '.' && (s[i+1] == 'e' || s[i+1] == 'E') {
		i++
	}
	if whole == "" && frac == "" {
		return 0
	}
	text := whole
	if whole == "" {
		text = "0"
	}
	if frac != "" {
		text += "." + frac
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		j := i + 1
		expSign := ""
		if j < len(s) && (s[j] == '+' || s[j] == '-') {
			expSign = s[j : j+1]
			j++
		}
		if exp, _ := scanDigits(s, j); exp != "" {
			text += "e" + expSign + exp
		}
	}
	f, _ := strconv.ParseFloat(sign+text, 64)
	return f
}

const (
	sextetBits  = 6
	nibbleBits  = 4
	pairBits    = 2
	byteMask    = 0xff
	noSextet    = -1
	base64Chars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
)

func sextet(c byte) int { return strings.IndexByte(base64Chars, c) }

// rubyDecode64 is Ruby's String#unpack("m"), which skips anything outside the alphabet and stops at a misplaced "=".
func rubyDecode64(s string) []byte {
	at := func(i int) byte {
		if i < len(s) {
			return s[i]
		}
		return 0
	}
	var out []byte
	i := 0
	for i < len(s) {
		a, b, c, d := noSextet, noSextet, noSextet, noSextet
		for ; ; i++ {
			if a = sextet(at(i)); a != noSextet || i >= len(s) {
				break
			}
		}
		if i >= len(s) {
			break
		}
		i++
		for ; ; i++ {
			if b = sextet(at(i)); b != noSextet || i >= len(s) {
				break
			}
		}
		if i >= len(s) {
			break
		}
		i++
		for ; ; i++ {
			if c = sextet(at(i)); c != noSextet || i >= len(s) || at(i) == '=' {
				break
			}
		}
		if at(i) == '=' || i >= len(s) {
			out = append(out, tailBytes(a, b, c)...)
			break
		}
		i++
		for ; ; i++ {
			if d = sextet(at(i)); d != noSextet || i >= len(s) || at(i) == '=' {
				break
			}
		}
		if at(i) == '=' || i >= len(s) {
			out = append(out, tailBytes(a, b, c)...)
			break
		}
		i++
		out = append(out, byte((a<<pairBits|b>>nibbleBits)&byteMask), byte((b<<nibbleBits|c>>pairBits)&byteMask), byte((c<<sextetBits|d)&byteMask))
	}
	return out
}

// tailBytes is what Ruby emits for a final group that ended early: a is always set, b too, c may be missing.
func tailBytes(a, b, c int) []byte {
	if a == noSextet || b == noSextet {
		return nil
	}
	first := byte((a<<pairBits | b>>nibbleBits) & byteMask)
	if c == noSextet {
		return []byte{first}
	}
	return []byte{first, byte((b<<nibbleBits | c>>pairBits) & byteMask)}
}

// value converts the node as the gem's to_ruby does, converting only the children Ruby does.
func (n *plistNode) value() (any, error) {
	switch n.tag {
	case "plist":
		if len(n.children) == 0 {
			return plistNil{}, nil
		}
		return n.children[0].value()
	case "dict":
		dict := &plistDict{}
		var key any
		for _, c := range n.children {
			v, err := c.value()
			if err != nil {
				return nil, err
			}
			if key == nil {
				if _, isNil := v.(plistNil); !isNil {
					key = v
				}
				continue
			}
			id, text, err := dictKey(key)
			if err != nil {
				id, key = "u:"+strconv.Itoa(len(dict.entries)), plistUnprintable{}
			}
			dict.set(id, text, key, v)
			key = nil
		}
		return dict, nil
	case "key", "string":
		return unescapeHTML(n.text), nil
	case "array":
		items := []any{}
		for _, c := range n.children {
			v, err := c.value()
			if err != nil {
				return nil, err
			}
			items = append(items, v)
		}
		return items, nil
	case "integer":
		return rubyToI(n.text), nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "real":
		return rubyToF(n.text), nil
	case "date":
		day, err := rubyDateTime(n.text, time.Now())
		if err != nil {
			return nil, err
		}
		return plistDate(day), nil
	case "data":
		return plistData(rubyDecode64(n.text)), nil
	}
	return nil, fmt.Errorf("unimplemented element %s in the plist", n.tag)
}

// dictKey gives a Ruby Hash key's identity and its to_s, which to_plist sorts and prints.
func dictKey(k any) (id, text string, err error) {
	switch x := k.(type) {
	case string:
		return "s:" + x, x, nil
	case *big.Int:
		return "i:" + x.String(), x.String(), nil
	case float64:
		return "f:" + rubyFloat(x), rubyFloat(x), nil
	case bool:
		return "b:" + strconv.FormatBool(x), strconv.FormatBool(x), nil
	case []any, *plistDict:
		text, err := rubyInspect(x)
		return "c:" + text, text, err
	}
	return "", "", errors.New("a dictionary key of this type is not supported")
}

// rubyInspect is Object#inspect for the values that print the same on every run.
func rubyInspect(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return inspectString(x), nil
	case *big.Int:
		return x.String(), nil
	case float64:
		return rubyFloat(x), nil
	case bool:
		return strconv.FormatBool(x), nil
	case plistNil:
		return "nil", nil
	case []any:
		parts := make([]string, 0, len(x))
		for _, e := range x {
			s, err := rubyInspect(e)
			if err != nil {
				return "", err
			}
			parts = append(parts, s)
		}
		return "[" + strings.Join(parts, ", ") + "]", nil
	case *plistDict:
		parts := make([]string, 0, len(x.entries))
		for _, e := range x.entries {
			val, err := rubyInspect(e.val)
			if err != nil {
				return "", err
			}
			key, err := rubyInspect(e.key)
			if err != nil {
				return "", err
			}
			parts = append(parts, key+" => "+val)
		}
		return "{" + strings.Join(parts, ", ") + "}", nil
	}
	return "", errors.New("a dictionary key of this type is not supported")
}

var inspectEscapes = map[rune]string{'\n': `\n`, '\t': `\t`, '\r': `\r`, '\f': `\f`, '\v': `\v`, '\a': `\a`, '\b': `\b`, 0x1b: `\e`, '"': `\"`, '\\': `\\`}

func inspectString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i, r := range s {
		switch {
		case inspectEscapes[r] != "":
			b.WriteString(inspectEscapes[r])
		case r == '#' && i+1 < len(s) && strings.ContainsRune("{$@", rune(s[i+1])):
			b.WriteString(`\#`)
		case r == utf8.RuneError:
			b.WriteString(`\x` + strings.ToUpper(strconv.FormatInt(int64(s[i]), 16)))
		case unicode.IsPrint(r):
			b.WriteRune(r)
		case r < asciiLimit:
			fmt.Fprintf(&b, `\x%02X`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04X`, r)
		default:
			fmt.Fprintf(&b, `\u{%X}`, r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

var htmlEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

const (
	dataLineWidth = 68
	marshalNilB64 = "BAgw"
	marshalNote   = "<!-- The <data> element below contains a Ruby object which has been serialized with Marshal.dump. -->\n"
)

func emitPlist(b *strings.Builder, v any, level int) {
	pad := strings.Repeat("\t", level)
	switch x := v.(type) {
	case *plistDict:
		if len(x.entries) == 0 {
			b.WriteString(pad + "<dict/>\n")
			return
		}
		b.WriteString(pad + "<dict>\n")
		entries := slices.Clone(x.entries)
		slices.SortStableFunc(entries, func(a, c dictEntry) int { return strings.Compare(a.text, c.text) })
		for _, e := range entries {
			emitScalar(b, strings.Repeat("\t", level+1), "key", htmlEscape.Replace(e.text))
			emitPlist(b, e.val, level+1)
		}
		b.WriteString(pad + "</dict>\n")
	case []any:
		if len(x) == 0 {
			b.WriteString(pad + "<array/>\n")
			return
		}
		b.WriteString(pad + "<array>\n")
		for _, e := range x {
			emitPlist(b, e, level+1)
		}
		b.WriteString(pad + "</array>\n")
	case bool:
		b.WriteString(pad + "<" + strconv.FormatBool(x) + "/>\n")
	case string:
		emitScalar(b, pad, "string", htmlEscape.Replace(x))
	case *big.Int:
		emitScalar(b, pad, "integer", x.String())
	case float64:
		emitScalar(b, pad, "real", rubyFloat(x))
	case plistDate:
		emitScalar(b, pad, "date", string(x))
	case plistNil:
		b.WriteString(marshalNote)
		emitData(b, pad, marshalNilB64)
	case plistData:
		emitData(b, pad, base64.StdEncoding.EncodeToString(x))
	}
}

func emitData(b *strings.Builder, pad, enc string) {
	b.WriteString(pad + "<data>\n")
	empty := enc == ""
	for len(enc) > 0 {
		n := min(dataLineWidth, len(enc))
		b.WriteString(pad + enc[:n] + "\n")
		enc = enc[n:]
	}
	if empty {
		b.WriteString("\n")
	}
	b.WriteString(pad + "</data>\n")
}

func emitScalar(b *strings.Builder, pad, tag, text string) {
	if text == "" {
		b.WriteString(pad + "<" + tag + "/>\n")
		return
	}
	b.WriteString(pad + "<" + tag + ">" + text + "</" + tag + ">\n")
}

func rubyFloat(f float64) string {
	switch {
	case f != f:
		return "NaN"
	case f > 1.7976931348623157e308:
		return "Infinity"
	case f < -1.7976931348623157e308:
		return "-Infinity"
	}
	abs := f
	if abs < 0 {
		abs = -abs
	}
	if abs == 0 || (abs >= 1e-4 && abs < 1e15) {
		s := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(s, ".") {
			s += ".0"
		}
		return s
	}
	s := strconv.FormatFloat(f, 'e', -1, 64)
	mantissa, exp, _ := strings.Cut(s, "e")
	if !strings.Contains(mantissa, ".") {
		mantissa += ".0"
	}
	return mantissa + "e" + exp
}
