package cask

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

func keyOrder(raw json.RawMessage) ([]string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	var keys []string
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := d.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

var plainSymbol = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*[?!=]?$`)

func rubySymbols(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		if plainSymbol.MatchString(n) {
			parts[i] = ":" + n
		} else {
			parts[i] = ":" + fmt.Sprintf("%q", n)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
