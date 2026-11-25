package rb

import (
	"strconv"
	"strings"
)

// rubyInteger is Kernel#Integer on a string: a sign, 0x/0b/0o/0d prefixes, a leading 0 for octal, and single underscores between digits.
func rubyInteger(s string) (int, bool) {
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
		return 0, false
	}
	n, err := strconv.ParseInt(strings.ReplaceAll(digits, "_", ""), base, 64)
	if err != nil {
		return 0, false
	}
	if negative {
		n = -n
	}
	return int(n), true
}
