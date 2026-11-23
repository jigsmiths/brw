package rb

import (
	"fmt"
	"strings"
	"unicode"
)

type kind int

const (
	tEOF kind = iota
	tNL
	tIdent
	tConst
	tLabel // key: in a hash or keyword arguments
	tSym
	tStr
	tWords
	tWordsI // %W[...]
	tSyms
	tInt
	tFloat
	tRegex
	tOp
	tKw
)

type strPart struct {
	lit    string
	interp []token // tokens of #{...}
	isExpr bool
}

type token struct {
	k         kind
	s         string
	parts     []strPart
	words     []string
	wordParts [][]strPart // %W words, which may interpolate
	spaceBef  bool
	line      int
}

var keywords = map[string]bool{
	"if": true, "unless": true, "else": true, "elsif": true, "end": true, "do": true, "then": true,
	"case": true, "when": true, "def": true, "nil": true, "true": true, "false": true, "self": true,
	"and": true, "or": true, "not": true, "return": true, "next": true, "while": true, "begin": true,
	"rescue": true, "ensure": true, "yield": true, "until": true, "in": true, "break": true,
	"class": true, "module": true,
}

type lexer struct {
	src      []rune
	i        int
	line     int
	toks     []token
	heredocs []*heredoc // pending, read at the next newline
}

type heredoc struct {
	term   string
	squig  bool
	dash   bool
	interp bool
	tok    int // index of the token to fill in
}

func lex(src string) ([]token, error) {
	l := &lexer{src: []rune(src), line: 1}
	if err := l.run(); err != nil {
		return nil, err
	}
	return l.toks, nil
}

func (l *lexer) peek(o int) rune {
	if l.i+o < len(l.src) && l.i+o >= 0 {
		return l.src[l.i+o]
	}
	return 0
}

func (l *lexer) emit(t token) { t.line = l.line; l.toks = append(l.toks, t) }

func (l *lexer) last() *token {
	if len(l.toks) == 0 {
		return nil
	}
	return &l.toks[len(l.toks)-1]
}

// continues reports whether a newline after the last token continues the expression.
func (l *lexer) continues() bool {
	t := l.last()
	if t == nil {
		return true
	}
	if t.k == tNL {
		return true
	}
	if t.k == tOp {
		switch t.s {
		case ")", "]", "}":
			return false
		}
		return true
	}
	if t.k == tKw && (t.s == "and" || t.s == "or" || t.s == "not") {
		return true
	}
	return false
}

func isIdentStart(r rune) bool { return r == '_' || unicode.IsLetter(r) }
func isIdent(r rune) bool      { return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r) }

func (l *lexer) run() error {
	for l.i < len(l.src) {
		c := l.peek(0)
		space := l.i > 0 && (l.src[l.i-1] == ' ' || l.src[l.i-1] == '\t' || l.src[l.i-1] == '\n')
		switch {
		case c == '\\' && l.peek(1) == '\n':
			l.i += 2
			l.line++
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case c == '#':
			for l.i < len(l.src) && l.src[l.i] != '\n' {
				l.i++
			}
		case c == '\n':
			l.i++
			if len(l.heredocs) > 0 {
				if err := l.readHeredocs(); err != nil {
					return err
				}
			}
			l.line++
			if !l.continues() {
				l.emit(token{k: tNL})
			}
		case c == '.' && l.peek(1) != '.' && l.last() != nil && l.last().k == tNL:
			// a leading-dot method chain continues the previous line
			l.toks = l.toks[:len(l.toks)-1]
			l.i++
			l.emit(token{k: tOp, s: ".", spaceBef: space})
		case c == '&' && l.peek(1) == '.':
			if l.last() != nil && l.last().k == tNL {
				l.toks = l.toks[:len(l.toks)-1]
			}
			l.i += 2
			l.emit(token{k: tOp, s: "&.", spaceBef: space})
		case unicode.IsDigit(c):
			l.number(space)
		case c == '@' && isIdentStart(l.peek(1)):
			// instance variables only appear in livecheck blocks, which are skipped
			l.i++
			if err := l.ident(space); err != nil {
				return err
			}
		case isIdentStart(c):
			if err := l.ident(space); err != nil {
				return err
			}
		case c == '"' || c == '\'' || c == '`':
			l.i++
			parts, err := l.str(c, c, c != '\'')
			if err != nil {
				return err
			}
			if l.peek(0) == ':' && l.peek(1) != ':' && c != '`' {
				l.i++
				l.emit(token{k: tLabel, s: joinLit(parts), spaceBef: space})
				continue
			}
			l.emit(token{k: tStr, parts: parts, spaceBef: space})
		case c == ':' && l.peek(1) == ':':
			l.i += 2
			l.emit(token{k: tOp, s: "::", spaceBef: space})
		case c == ':' && (isIdentStart(l.peek(1)) || l.peek(1) == '"'):
			l.i++
			if l.peek(0) == '"' {
				l.i++
				parts, err := l.str('"', '"', true)
				if err != nil {
					return err
				}
				l.emit(token{k: tSym, s: joinLit(parts), spaceBef: space})
				continue
			}
			st := l.i
			for l.i < len(l.src) && isIdent(l.src[l.i]) {
				l.i++
			}
			if r := l.peek(0); (r == '?' || r == '!' || r == '=') && l.peek(1) != '=' && l.peek(1) != '>' {
				l.i++
			}
			l.emit(token{k: tSym, s: string(l.src[st:l.i]), spaceBef: space})
		case c == '%' && l.peek(1) == 'W' && strings.ContainsRune("[({<|!/", l.peek(2)):
			open := l.peek(2)
			l.i += 3
			parts, err := l.str(open, closer(open), true)
			if err != nil {
				return err
			}
			l.emit(token{k: tWordsI, wordParts: splitWords(parts), spaceBef: space})
		case c == '%' && (l.peek(1) == 'w' || l.peek(1) == 'i' || l.peek(1) == 'r' || l.peek(1) == 'q' || l.peek(1) == 'Q') && strings.ContainsRune("[({<|!/", l.peek(2)):
			typ := l.peek(1)
			open := l.peek(2)
			l.i += 3
			cl := closer(open)
			if typ == 'r' {
				parts, err := l.str(open, cl, false)
				if err != nil {
					return err
				}
				for l.i < len(l.src) && strings.ContainsRune("imxo", l.src[l.i]) {
					l.i++
				}
				l.emit(token{k: tRegex, s: joinLit(parts), spaceBef: space})
				continue
			}
			parts, err := l.str(open, cl, typ == 'Q')
			if err != nil {
				return err
			}
			if typ == 'q' || typ == 'Q' {
				l.emit(token{k: tStr, parts: parts, spaceBef: space})
				continue
			}
			k := tWords
			if typ == 'i' {
				k = tSyms
			}
			l.emit(token{k: k, words: strings.Fields(joinLit(parts)), spaceBef: space})
		case c == '<' && l.peek(1) == '<' && (l.peek(2) == '~' || l.peek(2) == '-' || unicode.IsUpper(l.peek(2)) || l.peek(2) == '\''):
			l.i += 2
			h := &heredoc{interp: true}
			if l.peek(0) == '~' {
				h.squig = true
				l.i++
			} else if l.peek(0) == '-' {
				h.dash = true
				l.i++
			}
			quote := rune(0)
			if l.peek(0) == '\'' || l.peek(0) == '"' {
				quote = l.peek(0)
				h.interp = quote == '"'
				l.i++
			}
			st := l.i
			for l.i < len(l.src) && isIdent(l.src[l.i]) {
				l.i++
			}
			h.term = string(l.src[st:l.i])
			if quote != 0 {
				l.i++
			}
			l.emit(token{k: tStr, spaceBef: space})
			h.tok = len(l.toks) - 1
			l.heredocs = append(l.heredocs, h)
		case c == '/' && l.regexAllowed(space):
			l.i++
			parts, err := l.str('/', '/', false)
			if err != nil {
				return err
			}
			for l.i < len(l.src) && strings.ContainsRune("imxo", l.src[l.i]) {
				l.i++
			}
			l.emit(token{k: tRegex, s: joinLit(parts), spaceBef: space})
		default:
			op := ""
			for _, o := range []string{"**", "=>", "==", "!=", "=~", "!~", "<=>", "<=", ">=", "&&", "||", "<<", "->", "...", "..", "+=", "-=", "||=", "&&="} {
				if strings.HasPrefix(string(l.src[l.i:min(l.i+3, len(l.src))]), o) && len(o) > len(op) {
					op = o
				}
			}
			if op == "" {
				if !strings.ContainsRune("()[]{},.;+-*/%<>=!?:|&^~", c) {
					return fmt.Errorf("line %d: unexpected %q", l.line, c)
				}
				op = string(c)
			}
			l.i += len([]rune(op))
			if op == ";" {
				if !l.continues() {
					l.emit(token{k: tNL})
				}
				continue
			}
			l.emit(token{k: tOp, s: op, spaceBef: space})
		}
	}
	if len(l.heredocs) > 0 {
		return fmt.Errorf("unterminated heredoc")
	}
	l.emit(token{k: tNL})
	l.emit(token{k: tEOF})
	return nil
}

func closer(open rune) rune {
	switch open {
	case '[':
		return ']'
	case '(':
		return ')'
	case '{':
		return '}'
	case '<':
		return '>'
	}
	return open
}

func (l *lexer) regexAllowed(space bool) bool {
	t := l.last()
	if t == nil || t.k == tNL || t.k == tKw {
		return true
	}
	if t.k == tOp {
		return t.s != ")" && t.s != "]" && t.s != "}"
	}
	if t.k == tIdent && space && l.peek(1) != ' ' {
		return true
	}
	return false
}

func (l *lexer) number(space bool) {
	st := l.i
	if l.peek(0) == '0' && strings.ContainsRune("xXoObB", l.peek(1)) {
		// 0x1f, 0o755, 0b101: the parser reads the prefix
		l.i += 2
		for l.i < len(l.src) && (isIdent(l.src[l.i])) {
			l.i++
		}
		l.emit(token{k: tInt, s: strings.ReplaceAll(string(l.src[st:l.i]), "_", ""), spaceBef: space})
		return
	}
	isFloat := false
	for l.i < len(l.src) && (unicode.IsDigit(l.src[l.i]) || l.src[l.i] == '_' || (l.src[l.i] == '.' && unicode.IsDigit(l.peek(1)) && !isFloat)) {
		if l.src[l.i] == '.' {
			isFloat = true
		}
		l.i++
	}
	s := strings.ReplaceAll(string(l.src[st:l.i]), "_", "")
	if isFloat {
		l.emit(token{k: tFloat, s: s, spaceBef: space})
	} else {
		l.emit(token{k: tInt, s: s, spaceBef: space})
	}
}

func (l *lexer) ident(space bool) error {
	st := l.i
	for l.i < len(l.src) && isIdent(l.src[l.i]) {
		l.i++
	}
	if r := l.peek(0); (r == '?' || r == '!') && l.peek(1) != '=' {
		l.i++
	}
	s := string(l.src[st:l.i])
	if s == "__END__" {
		l.i = len(l.src)
		return nil
	}
	// label: `key: value` but not `a ? b : c` or `Foo::Bar`
	if l.peek(0) == ':' && l.peek(1) != ':' {
		last := l.last()
		if last == nil || last.k != tOp || last.s != "?" {
			l.i++
			l.emit(token{k: tLabel, s: s, spaceBef: space})
			return nil
		}
	}
	switch {
	case keywords[s]:
		l.emit(token{k: tKw, s: s, spaceBef: space})
	case unicode.IsUpper([]rune(s)[0]):
		l.emit(token{k: tConst, s: s, spaceBef: space})
	default:
		l.emit(token{k: tIdent, s: s, spaceBef: space})
	}
	return nil
}

// splitWords splits a %W literal at its unescaped whitespace.
func splitWords(parts []strPart) [][]strPart {
	var words [][]strPart
	var cur []strPart
	flush := func() {
		if len(cur) > 0 {
			words = append(words, cur)
			cur = nil
		}
	}
	for _, p := range parts {
		if p.isExpr {
			cur = append(cur, p)
			continue
		}
		start := -1
		for i, r := range p.lit {
			if unicode.IsSpace(r) {
				if start >= 0 {
					cur = append(cur, strPart{lit: p.lit[start:i]})
					start = -1
				}
				flush()
			} else if start < 0 {
				start = i
			}
		}
		if start >= 0 {
			cur = append(cur, strPart{lit: p.lit[start:]})
		}
	}
	flush()
	return words
}

func joinLit(parts []strPart) string {
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.lit)
	}
	return b.String()
}

// str reads a string body up to close, after the opening delimiter.
func (l *lexer) str(open, close rune, interp bool) ([]strPart, error) {
	var parts []strPart
	var b strings.Builder
	depth := 0
	for {
		if l.i >= len(l.src) {
			return nil, fmt.Errorf("line %d: unterminated string", l.line)
		}
		c := l.src[l.i]
		if c == '\n' {
			l.line++
		}
		switch {
		case !interp && (open == '/' || close == '}') && c == '#' && l.peek(1) == '{':
			// a regex's interpolation, kept as text
			d := 0
			for l.i < len(l.src) {
				r := l.src[l.i]
				b.WriteRune(r)
				l.i++
				if r == '{' {
					d++
				} else if r == '}' {
					d--
					if d == 0 {
						break
					}
				}
			}
		case c == '\\':
			n := l.peek(1)
			l.i += 2
			if !interp {
				// single quotes keep everything but \\ and \'; regexes keep it all
				if (n == close || n == '\\') && open != '/' && close != '/' {
					b.WriteRune(n)
				} else if n == close && (open == '/' || close == '/') {
					b.WriteRune(n)
				} else {
					b.WriteRune('\\')
					b.WriteRune(n)
				}
				continue
			}
			switch {
			case n >= '0' && n <= '7':
				// octal byte escape, up to three digits
				v := int(n - '0')
				for k := 0; k < 2 && l.peek(0) >= '0' && l.peek(0) <= '7'; k++ {
					v = v*8 + int(l.peek(0)-'0')
					l.i++
				}
				b.WriteByte(byte(v))
				continue
			case n == 'x':
				v := 0
				for k := 0; k < 2 && strings.ContainsRune("0123456789abcdefABCDEF", l.peek(0)); k++ {
					v = v*16 + hexVal(l.peek(0))
					l.i++
				}
				b.WriteByte(byte(v))
				continue
			case n == 'u':
				var digits string
				if l.peek(0) == '{' {
					l.i++
					for l.peek(0) != '}' {
						digits += string(l.peek(0))
						l.i++
					}
					l.i++
				} else {
					digits = string(l.src[l.i : l.i+4])
					l.i += 4
				}
				v := 0
				for _, d := range digits {
					v = v*16 + hexVal(d)
				}
				b.WriteRune(rune(v))
				continue
			}
			switch n {
			case 'n':
				b.WriteRune('\n')
			case 't':
				b.WriteRune('\t')
			case 'e':
				b.WriteRune('\x1b')
			case '0':
				b.WriteRune(0)
			case '\n':
			default:
				b.WriteRune(n)
			}
		case interp && c == '#' && l.peek(1) == '{':
			l.i += 2
			st := l.i
			d := 1
			for l.i < len(l.src) && d > 0 {
				switch l.src[l.i] {
				case '{':
					d++
				case '}':
					d--
				case '"':
					l.i++
					for l.i < len(l.src) && l.src[l.i] != '"' {
						if l.src[l.i] == '\\' {
							l.i++
						}
						l.i++
					}
				}
				l.i++
			}
			inner := string(l.src[st : l.i-1])
			toks, err := lex(inner)
			if err != nil {
				return nil, err
			}
			if b.Len() > 0 {
				parts = append(parts, strPart{lit: b.String()})
				b.Reset()
			}
			parts = append(parts, strPart{interp: toks, isExpr: true})
		case c == open && open != close:
			depth++
			b.WriteRune(c)
			l.i++
		case c == close:
			l.i++
			if depth > 0 {
				depth--
				b.WriteRune(c)
				continue
			}
			if b.Len() > 0 || len(parts) == 0 {
				parts = append(parts, strPart{lit: b.String()})
			}
			return parts, nil
		default:
			b.WriteRune(c)
			l.i++
		}
	}
}

func hexVal(r rune) int {
	switch {
	case r >= '0' && r <= '9':
		return int(r - '0')
	case r >= 'a' && r <= 'f':
		return int(r-'a') + 10
	case r >= 'A' && r <= 'F':
		return int(r-'A') + 10
	}
	return 0
}

// dedent removes a <<~ heredoc's common indentation as Ruby does: a tab reaches
// the next multiple of 8 columns, and one that would cross the indentation stays.
func dedent(lines []string) {
	width := func(ln string) (int, bool) {
		col := 0
		for _, r := range ln {
			switch r {
			case ' ':
				col++
			case '\t':
				col = (col/8 + 1) * 8
			default:
				return col, true
			}
		}
		return col, false
	}
	indent := -1
	for _, ln := range lines {
		if w, text := width(ln); text && (indent < 0 || w < indent) {
			indent = w
		}
	}
	if indent <= 0 {
		return
	}
	for i, ln := range lines {
		col, cut := 0, 0
		for _, r := range ln {
			next := col + 1
			if r == '\t' {
				next = (col/8 + 1) * 8
			} else if r != ' ' {
				break
			}
			if next > indent {
				break
			}
			col, cut = next, cut+1
		}
		lines[i] = ln[cut:]
	}
}

func (l *lexer) readHeredocs() error {
	for _, h := range l.heredocs {
		var lines []string
		for {
			if l.i >= len(l.src) {
				return fmt.Errorf("unterminated heredoc %s", h.term)
			}
			st := l.i
			for l.i < len(l.src) && l.src[l.i] != '\n' {
				l.i++
			}
			line := string(l.src[st:l.i])
			if l.i < len(l.src) {
				l.i++
			}
			l.line++
			if strings.TrimSpace(line) == h.term && (h.squig || h.dash || line == h.term) {
				break
			}
			lines = append(lines, line)
		}
		if h.squig {
			dedent(lines)
		}
		body := strings.Join(lines, "\n")
		if len(lines) > 0 {
			body += "\n"
		}
		sub := &lexer{src: []rune(body), line: 1}
		var parts []strPart
		var err error
		if h.interp {
			sub.src = append(sub.src, 0)
			parts, err = sub.str(-1, 0, true)
		} else {
			parts = []strPart{{lit: body}}
		}
		if err != nil {
			return err
		}
		l.toks[h.tok].parts = parts
	}
	l.heredocs = nil
	return nil
}
