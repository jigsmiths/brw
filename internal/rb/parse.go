package rb

import (
	"fmt"
	"strconv"
	"strings"
)

// node is a node of the syntax tree.
type node interface{ node() }

func (*litN) node()     {}
func (*strN) node()     {}
func (*symN) node()     {}
func (*regexN) node()   {}
func (*arrN) node()     {}
func (*hashN) node()    {}
func (*constN) node()   {}
func (*callN) node()    {}
func (*indexN) node()   {}
func (*binN) node()     {}
func (*notN) node()     {}
func (*negN) node()     {}
func (*ifN) node()      {}
func (*assignN) node()  {}
func (*setN) node()     {}
func (*classN) node()   {}
func (*defN) node()     {}
func (*caseN) node()    {}
func (*returnN) node()  {}
func (*splatN) node()   {}
func (*rescueN) node()  {}
func (*symProcN) node() {}
func (*beginN) node()   {}

type (
	litN   struct{ v value }
	strN   struct{ parts []node } // litN strings and expressions
	symN   struct{ s string }
	regexN struct{ src string }
	arrN   struct{ elems []node }
	pair   struct{ k, v node }
	hashN  struct{ pairs []pair }
	constN struct{ path []string }
	callN  struct {
		recv  node
		name  string
		args  []node // a trailing hashN holds keyword arguments
		block *blockN
		safe  bool
		bare  bool // a plain identifier: no receiver, arguments or parentheses
	}
	blockN struct {
		params []string
		body   []node
	}
	indexN struct {
		recv node
		args []node
	}
	binN struct {
		op   string
		l, r node
	}
	notN struct{ x node }
	negN struct{ x node }
	ifN  struct {
		cond      node
		then, els []node
	}
	assignN struct {
		name string
		v    node
	}
	// setN assigns to an index or attribute: ENV["X"] = v, obj.attr = v.
	setN struct {
		target node // *indexN or *callN
		v      node
	}
	classN struct {
		name, super string
		body        []node
	}
	param struct {
		name string
		def  node // default, or nil
		kw   bool
	}
	defN struct {
		name   string
		self   bool // def self.name
		params []param
		body   []node
	}
	when  struct{ pats, body []node }
	caseN struct {
		subject node // nil for a bare case
		whens   []when
		els     []node
	}
	returnN struct{ v node }
	splatN  struct{ x node }
	// rescueN is `x rescue fallback`.
	rescueN struct{ x, fallback node }
	// symProcN is a block argument such as &:to_s.
	symProcN struct{ name string }
	beginN   struct {
		body, rescue, ensure []node
		rescues              bool
	}
)

type parser struct {
	t []token
	i int
}

type parseErr struct{ msg string }

func (p *parser) fail(f string, a ...any) {
	panic(parseErr{fmt.Sprintf("line %d: ", p.cur().line) + fmt.Sprintf(f, a...)})
}

func parse(toks []token) (prog []node, err error) {
	defer func() {
		if r := recover(); r != nil {
			if pe, ok := r.(parseErr); ok {
				err = fmt.Errorf("parse: %s", pe.msg)
				return
			}
			panic(r)
		}
	}()
	p := &parser{t: toks}
	prog = p.stmts("")
	if p.cur().k != tEOF {
		p.fail("unexpected %v %q", p.cur().k, p.cur().s)
	}
	return prog, nil
}

func (p *parser) cur() token         { return p.t[p.i] }
func (p *parser) at(o int) token     { return p.t[min(p.i+o, len(p.t)-1)] }
func (p *parser) next() token        { t := p.t[p.i]; p.i++; return t }
func (p *parser) isOp(s string) bool { return p.cur().k == tOp && p.cur().s == s }
func (p *parser) isKw(s string) bool { return p.cur().k == tKw && p.cur().s == s }
func (p *parser) skipNL() {
	for p.cur().k == tNL {
		p.i++
	}
}
func (p *parser) expectOp(s string) {
	if !p.isOp(s) {
		p.fail("expected %q, got %q", s, p.cur().s)
	}
	p.i++
}

// stmts parses statements up to one of the given keywords (or "}" for brace blocks).
func (p *parser) stmts(end ...string) []node {
	var out []node
	for {
		p.skipNL()
		c := p.cur()
		if c.k == tEOF {
			return out
		}
		for _, e := range end {
			if (c.k == tKw && c.s == e) || (c.k == tOp && c.s == e) {
				return out
			}
		}
		out = append(out, p.stmt())
	}
}

func (p *parser) stmt() node {
	n := p.andOr()
	for {
		switch {
		case p.isKw("rescue"):
			p.i++
			n = &rescueN{n, p.andOr()}
		case p.isKw("if"):
			p.i++
			n = &ifN{cond: p.andOr(), then: []node{n}}
		case p.isKw("unless"):
			p.i++
			n = &ifN{cond: &notN{p.andOr()}, then: []node{n}}
		default:
			return n
		}
	}
}

// andOr joins expressions with the low-precedence and and or.
func (p *parser) andOr() node {
	n := p.notExpr()
	for p.isKw("and") || p.isKw("or") {
		op := map[string]string{"and": "&&", "or": "||"}[p.next().s]
		p.skipNL()
		n = &binN{op, n, p.notExpr()}
	}
	return n
}

// notExpr is an expression that may start with the keyword not.
func (p *parser) notExpr() node {
	if p.isKw("not") {
		p.i++
		return &notN{p.notExpr()}
	}
	return p.expr()
}

func (p *parser) expr() node {
	if p.cur().k == tIdent && p.at(1).k == tOp {
		switch op := p.at(1).s; op {
		case "=":
			name := p.next().s
			p.next()
			p.skipNL()
			return &assignN{name: name, v: p.expr()}
		case "+=", "-=", "||=", "&&=":
			name := p.next().s
			p.next()
			p.skipNL()
			return &assignN{name: name, v: &binN{op[:len(op)-1], &callN{name: name, bare: true}, p.expr()}}
		}
	}
	n := p.ternary()
	if p.isOp("=") {
		switch t := n.(type) {
		case *indexN:
		case *callN:
			if t.recv == nil || len(t.args) > 0 || t.block != nil {
				p.fail("assignment to %s", t.name)
			}
		default:
			p.fail("assignment")
		}
		p.i++
		p.skipNL()
		return &setN{target: n, v: p.expr()}
	}
	return n
}

func (p *parser) ternary() node {
	c := p.binary(0)
	if p.isOp("?") {
		p.i++
		p.skipNL()
		a := p.ternary()
		p.skipNL()
		if p.cur().k == tLabel { // `x ? y : z` lexed y: as a label
			p.fail("ternary label")
		}
		p.expectOp(":")
		p.skipNL()
		b := p.ternary()
		return &ifN{cond: c, then: []node{a}, els: []node{b}}
	}
	return c
}

var precedence = []map[string]bool{
	{"||": true},
	{"&&": true},
	{"==": true, "!=": true, "=~": true, "!~": true, "<=>": true},
	{"<": true, ">": true, "<=": true, ">=": true},
	{"..": true, "...": true},
	{"|": true, "^": true},
	{"&": true},
	{"<<": true},
	{"+": true, "-": true},
	{"*": true, "/": true, "%": true},
	{"**": true},
}

func (p *parser) binary(level int) node {
	if level == len(precedence) {
		return p.unary()
	}
	l := p.binary(level + 1)
	for p.cur().k == tOp && precedence[level][p.cur().s] {
		op := p.next().s
		if (op == ".." || op == "...") && (p.isOp("]") || p.isOp(")") || p.cur().k == tNL) {
			l = &binN{op, l, &litN{rint(-1)}} // endless range
			if op == "..." {
				p.fail("endless exclusive range")
			}
			continue
		}
		p.skipNL()
		l = &binN{op, l, p.binary(level + 1)}
	}
	return l
}

func (p *parser) unary() node {
	switch {
	case p.isOp("!"):
		p.i++
		return &notN{p.unary()}
	case p.isOp("-"):
		p.i++
		return &negN{p.unary()}
	}
	return p.postfix(p.primary())
}

func (p *parser) postfix(n node) node {
	for {
		switch {
		case p.isOp(".") || p.isOp("&."):
			safe := p.next().s == "&."
			t := p.next()
			if t.k != tIdent && t.k != tConst && t.k != tKw && t.k != tLabel {
				if t.k == tOp && t.s == "(" { // .() call
					p.fail("unsupported .()")
				}
				p.fail("expected method name, got %q", t.s)
			}
			c := &callN{recv: n, name: t.s, safe: safe}
			if t.k == tLabel { // `foo.bar key: v` lexed bar: as a label
				p.i--
				p.t[p.i] = token{k: tLabel, s: "", line: t.line}
				p.fail("method label")
			}
			p.callArgs(c, true)
			n = c
		case p.isOp("::"):
			p.i++
			t := p.next()
			if cn, ok := n.(*constN); ok && t.k == tConst {
				n = &constN{append(append([]string{}, cn.path...), t.s)}
				continue
			}
			c := &callN{recv: n, name: t.s}
			p.callArgs(c, true)
			n = c
		case p.isOp("[") && !p.cur().spaceBef:
			p.i++
			args := p.argList("]")
			p.expectOp("]")
			n = &indexN{n, args}
		default:
			return n
		}
	}
}

// startsArg reports whether the current token can start a command argument.
func (p *parser) startsArg() bool {
	c := p.cur()
	if !c.spaceBef {
		return false
	}
	switch c.k {
	case tStr, tSym, tInt, tFloat, tIdent, tConst, tLabel, tWords, tWordsI, tSyms, tRegex:
		return true
	case tKw:
		return c.s == "nil" || c.s == "true" || c.s == "false" || c.s == "self" || c.s == "not"
	case tOp:
		switch c.s {
		case "[", "->", "::", "!":
			return true
		case "-", "*", "&", ":":
			return !p.at(1).spaceBef
		}
	}
	return false
}

func (p *parser) callArgs(c *callN, allowCommand bool) {
	switch {
	case p.isOp("(") && !p.cur().spaceBef:
		p.i++
		c.args = p.argList(")")
		p.expectOp(")")
	case allowCommand && p.startsArg():
		c.args = p.argList("")
	default:
		if c.recv == nil {
			c.bare = true
		}
	}
	switch {
	case p.isKw("do") && c.recv == nil && (c.name == "livecheck" || c.name == "test"):
		// brw never runs livecheck or test blocks, so their Ruby only has to be skipped
		p.i++
		p.skipBlock()
		c.block = &blockN{}
		c.bare = false
	case p.isKw("do"):
		p.i++
		c.block = p.block("end")
		c.bare = false
	case p.isOp("{"):
		p.i++
		c.block = p.block("}")
		c.bare = false
	}
}

// skipBlock skips to the end that closes a do block, counting Ruby's openers.
func (p *parser) skipBlock() {
	depth := 1
	for depth > 0 {
		t := p.next()
		if t.k == tEOF {
			p.fail("unterminated block")
		}
		if t.k != tKw {
			continue
		}
		switch t.s {
		case "do", "case", "begin", "def", "class", "module":
			depth++
		case "if", "unless", "while", "until":
			prev := p.t[p.i-2]
			if prev.k == tNL || (prev.k == tOp && (prev.s == "=" || prev.s == "(" || prev.s == "||=" || prev.s == "|")) || (prev.k == tKw && (prev.s == "then" || prev.s == "else" || prev.s == "do")) {
				depth++
			}
		case "end":
			depth--
		}
	}
}

func (p *parser) block(end string) *blockN {
	b := &blockN{}
	p.skipNL()
	if p.isOp("|") {
		p.i++
		for !p.isOp("|") {
			t := p.next()
			if t.k == tIdent {
				b.params = append(b.params, t.s)
			}
		}
		p.i++
	}
	b.body = p.stmts(end)
	if end == "end" {
		if !p.isKw("end") {
			p.fail("expected end")
		}
	} else {
		p.expectOp(end)
		return b
	}
	p.i++
	return b
}

// argList collects `key: v` and `k => v` pairs into a trailing hash, as Ruby does.
func (p *parser) argList(close string) []node {
	var args []node
	var h *hashN
	bracketed := close != ""
	for {
		if bracketed {
			p.skipNL()
			if p.isOp(close) {
				break
			}
		}
		if p.cur().k == tLabel {
			if h == nil {
				h = &hashN{}
			}
			k := p.next().s
			if bracketed {
				p.skipNL()
			}
			if p.isOp(",") || (bracketed && p.isOp(close)) { // shorthand `key:`
				h.pairs = append(h.pairs, pair{&symN{k}, &callN{name: k, bare: true}})
			} else {
				h.pairs = append(h.pairs, pair{&symN{k}, p.expr()})
			}
		} else if p.isOp("*") {
			p.i++
			args = append(args, &splatN{p.ternary()})
		} else if p.isOp("&") && p.at(1).k == tSym {
			p.i++
			args = append(args, &symProcN{p.next().s})
		} else if p.isOp("&") || p.isOp("**") {
			p.fail("unsupported block argument or double splat")
		} else {
			e := p.expr()
			if p.isOp("=>") {
				p.i++
				p.skipNL()
				if h == nil {
					h = &hashN{}
				}
				h.pairs = append(h.pairs, pair{e, p.expr()})
			} else {
				args = append(args, e)
			}
		}
		if bracketed {
			p.skipNL()
		}
		if !p.isOp(",") {
			break
		}
		p.i++
		p.skipNL()
	}
	if h != nil {
		args = append(args, h)
	}
	return args
}

func (p *parser) primary() node {
	t := p.cur()
	switch t.k {
	case tInt:
		p.i++
		// base 0 reads Ruby's literals: 0755 and 0o755 octal, 0x hex, 0b binary
		v, err := strconv.ParseInt(t.s, 0, 64)
		if err != nil {
			p.fail("integer %s", t.s)
		}
		return &litN{rint(v)}
	case tFloat:
		p.i++
		v, err := strconv.ParseFloat(t.s, 64)
		if err != nil {
			p.fail("float %s", t.s)
		}
		return &litN{rfloat(v)}
	case tStr:
		p.i++
		n := p.strNode(t.parts)
		// adjacent string literals concatenate
		for p.cur().k == tStr && p.cur().spaceBef {
			n.parts = append(n.parts, p.strNode(p.next().parts).parts...)
		}
		return n
	case tSym:
		p.i++
		return &symN{t.s}
	case tRegex:
		p.i++
		return &regexN{t.s}
	case tWords:
		p.i++
		a := &arrN{}
		for _, w := range t.words {
			a.elems = append(a.elems, &litN{rstr(w)})
		}
		return a
	case tWordsI:
		p.i++
		a := &arrN{}
		for _, w := range t.wordParts {
			a.elems = append(a.elems, p.strNode(w))
		}
		return a
	case tSyms:
		p.i++
		a := &arrN{}
		for _, w := range t.words {
			a.elems = append(a.elems, &symN{w})
		}
		return a
	case tConst:
		p.i++
		if p.isOp("(") && !p.cur().spaceBef {
			c := &callN{name: t.s}
			p.callArgs(c, false)
			return c
		}
		return &constN{[]string{t.s}}
	case tIdent:
		p.i++
		c := &callN{name: t.s}
		p.callArgs(c, true)
		return c
	case tLabel:
		// a bare hash in a position where only arguments are expected
		args := p.argList("")
		if len(args) == 1 {
			return args[0]
		}
		p.fail("unexpected label")
	case tKw:
		switch t.s {
		case "nil":
			p.i++
			return &litN{nil}
		case "true":
			p.i++
			return &litN{rbool(true)}
		case "false":
			p.i++
			return &litN{rbool(false)}
		case "self":
			p.i++
			return &callN{name: "self", bare: true}
		case "if", "unless":
			p.i++
			return p.ifExpr(t.s == "unless")
		case "class":
			p.i++
			return p.classDef()
		case "begin":
			p.i++
			b := &beginN{body: p.stmts("rescue", "ensure", "end")}
			if p.isKw("rescue") {
				p.i++
				if p.cur().k == tConst || p.isOp("=>") {
					p.fail("rescue of a named exception")
				}
				b.rescues = true
				b.rescue = p.stmts("ensure", "end")
			}
			if p.isKw("ensure") {
				p.i++
				b.ensure = p.stmts("end")
			}
			if !p.isKw("end") {
				p.fail("expected end")
			}
			p.i++
			return b
		case "def":
			p.i++
			return p.methodDef()
		case "case":
			p.i++
			return p.caseExpr()
		case "return":
			p.i++
			r := &returnN{}
			if !p.isKw("if") && !p.isKw("unless") && p.cur().k != tNL && p.cur().k != tEOF && !p.isKw("end") {
				r.v = p.expr()
			}
			return r
		default:
			p.fail("unsupported %s", t.s)
		}
	case tOp:
		switch t.s {
		case "(":
			p.i++
			p.skipNL()
			e := p.stmt()
			p.skipNL()
			p.expectOp(")")
			return e
		case "[":
			p.i++
			elems := p.argList("]")
			p.expectOp("]")
			return &arrN{elems}
		case "{":
			p.i++
			args := p.argList("}")
			p.expectOp("}")
			if len(args) == 0 {
				return &hashN{}
			}
			if h, ok := args[len(args)-1].(*hashN); ok && len(args) == 1 {
				return h
			}
			p.fail("bad hash literal")
		case "->":
			p.fail("unsupported lambda")
		case "::":
			p.i++
			return &constN{[]string{p.next().s}}
		}
	}
	p.fail("unexpected %q", t.s)
	return nil
}

func (p *parser) constPath() string {
	if p.cur().k != tConst {
		p.fail("expected a constant, got %q", p.cur().s)
	}
	var name strings.Builder
	name.WriteString(p.next().s)
	for p.isOp("::") {
		p.i++
		if p.cur().k != tConst {
			p.fail("expected a constant, got %q", p.cur().s)
		}
		name.WriteString("::" + p.next().s)
	}
	return name.String()
}

// classDef parses `class Name < Super ... end`.
func (p *parser) classDef() node {
	c := &classN{name: p.constPath()}
	if p.isOp("<") {
		p.i++
		c.super = p.constPath()
	}
	c.body = p.stmts("end")
	if !p.isKw("end") {
		p.fail("expected end")
	}
	p.i++
	return c
}

// methodDef parses `def name(params) ... end`, with or without parentheses.
func (p *parser) methodDef() node {
	t := p.next()
	self := t.k == tKw && t.s == "self" && p.isOp(".")
	if self {
		p.i++
		t = p.next()
	}
	if t.k != tIdent && t.k != tConst && t.k != tKw {
		p.fail("method name %q", t.s)
	}
	d := &defN{name: t.s, self: self}
	if p.isOp("=") {
		p.fail("endless method")
	}
	parens := p.isOp("(") && !p.cur().spaceBef
	if parens {
		p.i++
	}
	closed := func() bool { return parens && p.isOp(")") }
	for !closed() && p.cur().k != tNL && !p.isOp(";") {
		switch c := p.next(); c.k {
		case tIdent:
			pr := param{name: c.s}
			if p.isOp("=") {
				p.i++
				pr.def = p.ternary()
			}
			d.params = append(d.params, pr)
		case tLabel:
			pr := param{name: c.s, kw: true}
			if !p.isOp(",") && !closed() && p.cur().k != tNL {
				pr.def = p.ternary()
			}
			d.params = append(d.params, pr)
		default:
			p.fail("parameter %q", c.s)
		}
		if p.isOp(",") {
			p.i++
			p.skipNL()
		}
	}
	if parens {
		p.expectOp(")")
	}
	d.body = p.stmts("end")
	if !p.isKw("end") {
		p.fail("expected end")
	}
	p.i++
	return d
}

func (p *parser) caseExpr() node {
	c := &caseN{}
	if p.cur().k != tNL {
		c.subject = p.expr()
	}
	p.skipNL()
	for p.isKw("when") {
		p.i++
		var w when
		for {
			p.skipNL()
			w.pats = append(w.pats, p.ternary())
			if !p.isOp(",") {
				break
			}
			p.i++
		}
		if p.isKw("then") {
			p.i++
		}
		w.body = p.stmts("when", "else", "end")
		c.whens = append(c.whens, w)
	}
	if p.isKw("else") {
		p.i++
		c.els = p.stmts("end")
	}
	if !p.isKw("end") {
		p.fail("expected end")
	}
	p.i++
	return c
}

func (p *parser) ifExpr(negate bool) node {
	cond := p.andOr()
	if negate {
		cond = &notN{cond}
	}
	if p.isKw("then") {
		p.i++
	}
	n := &ifN{cond: cond, then: p.stmts("elsif", "else", "end")}
	switch {
	case p.isKw("elsif"):
		p.i++
		n.els = []node{p.ifExpr(false)}
		return n
	case p.isKw("else"):
		p.i++
		n.els = p.stmts("end")
	}
	if !p.isKw("end") {
		p.fail("expected end of if")
	}
	p.i++
	return n
}

func (p *parser) strNode(parts []strPart) *strN {
	s := &strN{}
	for _, pt := range parts {
		if !pt.isExpr {
			s.parts = append(s.parts, &litN{rstr(pt.lit)})
			continue
		}
		sub := &parser{t: pt.interp}
		body := sub.stmts()
		if len(body) != 1 {
			p.fail("interpolation with %d statements", len(body))
		}
		s.parts = append(s.parts, body[0])
	}
	return s
}
