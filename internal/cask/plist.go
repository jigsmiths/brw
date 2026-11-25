package cask

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

const plistHeader = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
`

const marshalledNilComment = "<!-- The <data> element below contains a Ruby object which has been serialized with Marshal.dump. -->\n"

// marshalledNil is Ruby's Marshal.dump(nil), base64 encoded.
const marshalledNil = "BAgw"

var integerText = regexp.MustCompile(`^-?\d+$`)

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")

func plistDump(jsonValue []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(jsonValue))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	var b strings.Builder
	b.WriteString(plistHeader)
	if err := plistNode(&b, v, 0); err != nil {
		return nil, err
	}
	b.WriteString("</plist>\n")
	return []byte(b.String()), nil
}

func plistNode(b *strings.Builder, v any, level int) error {
	pad := strings.Repeat("\t", level)
	leaf := func(tag, text string) {
		if text == "" {
			fmt.Fprintf(b, "%s<%s/>\n", pad, tag)
			return
		}
		fmt.Fprintf(b, "%s<%s>%s</%s>\n", pad, tag, text, tag)
	}
	switch t := v.(type) {
	case []any:
		if len(t) == 0 {
			leaf("array", "")
			return nil
		}
		b.WriteString(pad + "<array>\n")
		for _, e := range t {
			if err := plistNode(b, e, level+1); err != nil {
				return err
			}
		}
		b.WriteString(pad + "</array>\n")
	case map[string]any:
		if len(t) == 0 {
			leaf("dict", "")
			return nil
		}
		b.WriteString(pad + "<dict>\n")
		for _, k := range slices.Sorted(maps.Keys(t)) {
			fmt.Fprintf(b, "%s\t<key>%s</key>\n", pad, htmlEscaper.Replace(k))
			if err := plistNode(b, t[k], level+1); err != nil {
				return err
			}
		}
		b.WriteString(pad + "</dict>\n")
	case bool:
		leaf(strconv.FormatBool(t), "")
	case string:
		leaf("string", htmlEscaper.Replace(t))
	case json.Number:
		if integerText.MatchString(t.String()) {
			leaf("integer", t.String())
			return nil
		}
		f, err := t.Float64()
		if err != nil {
			return err
		}
		text := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		leaf("real", text)
	case nil:
		b.WriteString(marshalledNilComment)
		fmt.Fprintf(b, "%s<data>\n%s%s\n%s</data>\n", pad, pad, marshalledNil, pad)
	default:
		return fmt.Errorf("unexpected %T in a plist", v)
	}
	return nil
}

func plistDictStrings(data []byte) (map[string]string, error) {
	d := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := d.Token()
		if errors.Is(err, io.EOF) {
			return nil, errors.New("no dict in the plist")
		}
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "dict" {
			break
		}
	}
	out := map[string]string{}
	key := ""
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.EndElement:
			return out, nil
		case xml.StartElement:
			var text string
			switch t.Name.Local {
			case "key":
				if err := d.DecodeElement(&key, &t); err != nil {
					return nil, err
				}
			case "string":
				if err := d.DecodeElement(&text, &t); err != nil {
					return nil, err
				}
				out[key] = text
			default:
				if err := d.Skip(); err != nil {
					return nil, err
				}
			}
		}
	}
}
