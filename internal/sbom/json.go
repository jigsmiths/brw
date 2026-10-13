package sbom

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// node is a JSON value that keeps the order of an object's keys, which a Go map
// would lose and which Ruby's JSON, as brew uses it, keeps.
type node struct {
	kind  byte // 'o'bject, 'a'rray, 's'tring, or 'l'iteral for numbers, true, false and null
	keys  []string
	vals  []*node // of an object, parallel to keys
	items []*node // of an array
	text  string  // of a string, decoded; of a literal, as written
}

func parse(data []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	n, err := parseValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("text after the JSON value")
	}
	return n, nil
}

func parseValue(dec *json.Decoder) (*node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		if t == '{' {
			n := &node{kind: 'o'}
			for dec.More() {
				key, err := dec.Token()
				if err != nil {
					return nil, err
				}
				v, err := parseValue(dec)
				if err != nil {
					return nil, err
				}
				n.set(key.(string), v)
			}
			_, err := dec.Token()
			return n, err
		}
		n := &node{kind: 'a'}
		for dec.More() {
			v, err := parseValue(dec)
			if err != nil {
				return nil, err
			}
			n.items = append(n.items, v)
		}
		_, err := dec.Token()
		return n, err
	case string:
		return &node{kind: 's', text: t}, nil
	case json.Number:
		return &node{kind: 'l', text: t.String()}, nil
	case bool:
		if t {
			return &node{kind: 'l', text: "true"}, nil
		}
		return &node{kind: 'l', text: "false"}, nil
	}
	return &node{kind: 'l', text: "null"}, nil
}

// set gives key the value. A key that is there keeps its place, as in Ruby.
func (n *node) set(key string, v *node) {
	for i, k := range n.keys {
		if k == key {
			n.vals[i] = v
			return
		}
	}
	n.keys = append(n.keys, key)
	n.vals = append(n.vals, v)
}

func (n *node) get(key string) *node {
	for i, k := range n.keys {
		if k == key {
			return n.vals[i]
		}
	}
	return nil
}

func (n *node) isObject() bool { return n != nil && n.kind == 'o' }
func (n *node) isArray() bool  { return n != nil && n.kind == 'a' }

// pretty writes the value the way Ruby's JSON.pretty_generate does: two spaces,
// "key": value, empty lists and objects as [] and {}, no final newline.
func (n *node) pretty() []byte {
	var b bytes.Buffer
	n.write(&b, 0)
	return b.Bytes()
}

func (n *node) write(b *bytes.Buffer, depth int) {
	indent := strings.Repeat("  ", depth+1)
	switch n.kind {
	case 'o':
		if len(n.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range n.keys {
			b.WriteString(indent)
			writeString(b, k)
			b.WriteString(": ")
			n.vals[i].write(b, depth+1)
			if i < len(n.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(strings.Repeat("  ", depth) + "}")
	case 'a':
		if len(n.items) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, v := range n.items {
			b.WriteString(indent)
			v.write(b, depth+1)
			if i < len(n.items)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		b.WriteString(strings.Repeat("  ", depth) + "]")
	case 's':
		writeString(b, n.text)
	default:
		b.WriteString(n.text)
	}
}

// writeString escapes like Ruby's JSON generator: quotes, backslashes and
// control characters, and nothing else. Slashes, angle brackets, DEL and
// U+2028 stay as they are.
func writeString(b *bytes.Buffer, s string) {
	const hex = "0123456789abcdef"
	b.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		switch {
		case c == '"':
			b.WriteString(`\"`)
		case c == '\\':
			b.WriteString(`\\`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hex[c>>4])
			b.WriteByte(hex[c&0xf])
		case c < utf8.RuneSelf:
			b.WriteByte(c)
		default:
			r, size := utf8.DecodeRuneInString(s[i:])
			b.WriteRune(r)
			i += size
			continue
		}
		i++
	}
	b.WriteByte('"')
}
