package rb

import (
	"math/big"
	"strings"
)

func rubyInteger(s string) (*big.Int, bool) {
	s = strings.TrimSpace(s)
	negative := strings.HasPrefix(s, "-")
	if negative || strings.HasPrefix(s, "+") {
		s = s[1:]
	}
	base, digits := 10, s
	if len(s) > 1 && s[0] == '0' {
		switch s[1] {
		case 'x', 'X':
			base, digits = 16, s[2:]
		case 'b', 'B':
			base, digits = 2, s[2:]
		case 'o', 'O':
			base, digits = 8, s[2:]
		case 'd', 'D':
			base, digits = 10, s[2:]
		default:
			base = 8
		}
	}
	if digits == "" || digits[0] == '_' || digits[len(digits)-1] == '_' || strings.Contains(digits, "__") {
		return nil, false
	}
	n, ok := new(big.Int).SetString(strings.ReplaceAll(digits, "_", ""), base)
	if !ok {
		return nil, false
	}
	if negative {
		n.Neg(n)
	}
	return n, true
}
