package services

import (
	"encoding/base64"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const userNameKeyName = "UserName"

// ApplyUserName sets UserName and re-serializes the plist as the Ruby plist gem's to_plist does.
func ApplyUserName(plist []byte, user string) ([]byte, error) {
	root, err := parsePlist(string(plist))
	if err != nil {
		return nil, err
	}
	dict, ok := root.(map[string]any)
	if !ok {
		return nil, errors.New("the plist is not a dictionary")
	}
	dict[userNameKeyName] = user
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

type plistNode struct {
	tag      string
	text     string
	children []any
}

var (
	plistTags = "plist|dict|key|string|array|integer|true|false|real|date|data"
	startTag  = regexp.MustCompile(`(?i)\A<(` + plistTags + `)([^>]*)>`)
	endTag    = regexp.MustCompile(`(?i)\A</(` + plistTags + `)[^>]*>`)
	xmlDecl   = regexp.MustCompile(`(?s)\A<\?xml\s+(.*?)\?>*`)
	doctype   = regexp.MustCompile(`(?s)\A\s*<!DOCTYPE\s+(.*?)(\[|>)`)
	cdata     = regexp.MustCompile(`(?s)\A<!\[CDATA\[(.*?)\]\]>`)
	comment   = regexp.MustCompile(`(?s)\A<!--.*?-->`)
)

// parsePlist follows the Ruby plist gem's stream parser with marshal off, including what it rejects.
func parsePlist(src string) (any, error) {
	var open []*plistNode
	var result any
	done := false
	for rest := src; rest != ""; {
		if loc := comment.FindStringIndex(rest); loc != nil {
			rest = rest[loc[1]:]
		} else if loc := xmlDecl.FindStringIndex(rest); loc != nil {
			rest = rest[loc[1]:]
		} else if loc := doctype.FindStringIndex(rest); loc != nil {
			rest = rest[loc[1]:]
		} else if m := startTag.FindStringSubmatch(rest); m != nil {
			rest = rest[len(m[0]):]
			open = append(open, &plistNode{tag: strings.ToLower(m[1])})
			if strings.HasSuffix(m[2], "/") {
				var err error
				if open, result, done, err = closeNode(open, result, done); err != nil {
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
			var err error
			if open, result, done, err = closeNode(open, result, done); err != nil {
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

func closeNode(open []*plistNode, result any, done bool) ([]*plistNode, any, bool, error) {
	if len(open) == 0 {
		return open, result, done, errors.New("unbalanced end tag in the plist")
	}
	last := open[len(open)-1]
	open = open[:len(open)-1]
	value, err := last.value()
	if err != nil {
		return open, result, done, err
	}
	if len(open) == 0 {
		return open, value, true, nil
	}
	parent := open[len(open)-1]
	parent.children = append(parent.children, value)
	return open, result, done, nil
}

type plistKey string

var (
	leadingInt   = regexp.MustCompile(`\A\s*[+-]?\d+`)
	leadingFloat = regexp.MustCompile(`\A\s*[+-]?(\d+(\.\d+)?([eE][+-]?\d+)?|\.\d+)`)
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

func (n *plistNode) value() (any, error) {
	switch n.tag {
	case "plist":
		if len(n.children) == 0 {
			return nil, nil
		}
		return n.children[0], nil
	case "dict":
		dict := map[string]any{}
		var key *string
		for _, c := range n.children {
			if key == nil {
				k, ok := c.(plistKey)
				if !ok {
					return nil, errors.New("a dictionary key is not a key")
				}
				s := string(k)
				key = &s
				continue
			}
			dict[*key] = c
			key = nil
		}
		return dict, nil
	case "key":
		return plistKey(unescapeHTML(n.text)), nil
	case "string":
		return unescapeHTML(n.text), nil
	case "array":
		return append([]any{}, n.children...), nil
	case "integer":
		i, _ := new(big.Int).SetString(strings.ReplaceAll(strings.TrimSpace(leadingInt.FindString(n.text)), "+", ""), 10)
		if i == nil {
			i = new(big.Int)
		}
		return i, nil
	case "true":
		return true, nil
	case "false":
		return false, nil
	case "real":
		f, _ := strconv.ParseFloat(strings.TrimSpace(leadingFloat.FindString(n.text)), 64)
		return f, nil
	case "date":
		t, err := time.Parse("2006-01-02T15:04:05Z07:00", strings.TrimSpace(n.text))
		if err != nil {
			return nil, fmt.Errorf("unreadable date %q in the plist", n.text)
		}
		return plistDate(t.Format("2006-01-02T15:04:05Z")), nil
	case "data":
		raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(n.text), ""))
		if err != nil {
			raw, _ = base64.RawStdEncoding.DecodeString(strings.Join(strings.Fields(n.text), ""))
		}
		return plistData(raw), nil
	}
	return nil, fmt.Errorf("unimplemented element %s in the plist", n.tag)
}

var htmlEscape = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

const dataLineWidth = 68

func emitPlist(b *strings.Builder, v any, level int) {
	pad := strings.Repeat("\t", level)
	switch x := v.(type) {
	case map[string]any:
		if len(x) == 0 {
			b.WriteString(pad + "<dict/>\n")
			return
		}
		b.WriteString(pad + "<dict>\n")
		for _, k := range slices.Sorted(maps.Keys(x)) {
			b.WriteString(strings.Repeat("\t", level+1) + "<key>" + htmlEscape.Replace(k) + "</key>\n")
			emitPlist(b, x[k], level+1)
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
	case plistData:
		b.WriteString(pad + "<data>\n")
		enc := base64.StdEncoding.EncodeToString(x)
		for len(enc) > 0 {
			n := min(dataLineWidth, len(enc))
			b.WriteString(pad + enc[:n] + "\n")
			enc = enc[n:]
		}
		if len(x) == 0 {
			b.WriteString("\n")
		}
		b.WriteString(pad + "</data>\n")
	}
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
	if abs == 0 || (abs >= 1e-4 && abs < 1e16) {
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
