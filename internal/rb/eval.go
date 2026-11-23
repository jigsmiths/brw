package rb

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// value is a Ruby value. Ruby's nil is a nil value.
type value interface{ rubyValue() }

type (
	rstr   string
	rint   int64
	rfloat float64
	rbool  bool
	sym    string
	ver    string // a cask version: a string with version helpers
	regex  struct{ src string }
	hash   struct {
		keys []value
		vals []value
	}
	arr   struct{ v []value }
	tmplV struct{} // version inside install steps
	proc  struct {
		b   *blockN
		env *scope
		sym string // &:name, which calls name on its argument
	}
	special string // a constant such as MacOS
)

func (rstr) rubyValue()    {}
func (rint) rubyValue()    {}
func (rfloat) rubyValue()  {}
func (rbool) rubyValue()   {}
func (sym) rubyValue()     {}
func (ver) rubyValue()     {}
func (*regex) rubyValue()  {}
func (*hash) rubyValue()   {}
func (*arr) rubyValue()    {}
func (tmplV) rubyValue()   {}
func (*proc) rubyValue()   {}
func (special) rubyValue() {}
func (*macVer) rubyValue() {}
func (*rangeV) rubyValue() {}
func (caskRef) rubyValue() {}
func (*guardV) rubyValue() {}

// MarshalJSON writes what Ruby's to_json would.
func (s sym) MarshalJSON() ([]byte, error)    { return json.Marshal(string(s)) }
func (v ver) MarshalJSON() ([]byte, error)    { return json.Marshal(string(v)) }
func (r *regex) MarshalJSON() ([]byte, error) { return json.Marshal("(?-mix:" + r.src + ")") }
func (tmplV) MarshalJSON() ([]byte, error)    { return json.Marshal("{{version}}") }

// MarshalJSON writes an empty array as [], not null.
func (a *arr) MarshalJSON() ([]byte, error) {
	if a.v == nil {
		return []byte("[]"), nil
	}
	return json.Marshal(a.v)
}

func (h *hash) get(k value) (value, bool) {
	for i, kk := range h.keys {
		if eq(kk, k) {
			return h.vals[i], true
		}
	}
	return nil, false
}

func (h *hash) set(k, v value) {
	for i, kk := range h.keys {
		if eq(kk, k) {
			h.vals[i] = v
			return
		}
	}
	h.keys = append(h.keys, k)
	h.vals = append(h.vals, v)
}

func (h *hash) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range h.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, _ := json.Marshal(str(k))
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(h.vals[i])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

func (h *hash) put(k string, v value) { h.set(rstr(k), v) }

func strs(ss []string) *arr {
	a := &arr{v: []value{}}
	for _, s := range ss {
		a.v = append(a.v, rstr(s))
	}
	return a
}

func eq(a, b value) bool {
	switch x := a.(type) {
	case ver:
		if y, ok := b.(rstr); ok {
			return string(x) == string(y)
		}
		if y, ok := b.(ver); ok {
			return x == y
		}
		return false
	case rstr:
		if y, ok := b.(ver); ok {
			return string(x) == string(y)
		}
	case rint:
		if y, ok := b.(rfloat); ok {
			return rfloat(x) == y
		}
	}
	return a == b // every value type is comparable
}

func str(v value) string {
	switch x := v.(type) {
	case nil:
		return ""
	case caskRef:
		return x.b.token
	case rstr:
		return string(x)
	case sym:
		return string(x)
	case ver:
		return string(x)
	case rint:
		return strconv.FormatInt(int64(x), 10)
	case rfloat:
		return strconv.FormatFloat(float64(x), 'f', -1, 64)
	case rbool:
		return strconv.FormatBool(bool(x))
	case tmplV:
		return "{{version}}"
	case *buffer:
		return x.s
	}
	fail("%T as a string", v)
	return ""
}

func truthy(v value) bool {
	switch x := v.(type) {
	case nil:
		return false
	case rbool:
		return bool(x)
	}
	return true
}

type unsupported struct{ what string }

func (u unsupported) Error() string { return "unsupported: " + u.what }

func (u unsupported) Unwrap() error { return ErrUnsupported }

// caskRef is @cask in a caveat: the cask being loaded.
type caskRef struct{ b *caskB }

func fail(f string, a ...any) { panic(unsupported{fmt.Sprintf(f, a...)}) }

type scope struct {
	vars   map[string]value
	parent *scope
}

func (s *scope) lookup(n string) (value, bool) {
	for c := s; c != nil; c = c.parent {
		if v, ok := c.vars[n]; ok {
			return v, true
		}
	}
	return nil, false
}

func (s *scope) assign(n string, v value) {
	for c := s; c != nil; c = c.parent {
		if _, ok := c.vars[n]; ok {
			c.vars[n] = v
			return
		}
	}
	s.vars[n] = v
}

// dsl handles calls with no receiver that aren't local variables.
type dsl interface {
	call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool)
}

type interp struct {
	plat  *platform
	ctx   []dsl
	build *builder // set while a formula's install method runs
}

func (in *interp) run(body []node, s *scope) value {
	var last value
	for _, n := range body {
		last = in.eval(n, s)
	}
	return last
}

// returned unwinds a method body to its caller with the value of a return.
type returned struct{ v value }

// callDef runs a method defined in the file.
func (in *interp) callDef(d *defN, args []value, kw *hash, s *scope) (out value) {
	ms := &scope{vars: map[string]value{}, parent: s}
	pos := 0
	for _, p := range d.params {
		switch {
		case p.kw:
			v, ok := value(nil), false
			if kw != nil {
				v, ok = kw.get(sym(p.name))
			}
			if !ok {
				if p.def == nil {
					fail("missing keyword %s for %s", p.name, d.name)
				}
				v = in.eval(p.def, ms)
			}
			ms.vars[p.name] = v
		case pos < len(args):
			ms.vars[p.name] = args[pos]
			pos++
		case p.def != nil:
			ms.vars[p.name] = in.eval(p.def, ms)
		default:
			fail("too few arguments to %s", d.name)
		}
	}
	if pos < len(args) {
		fail("too many arguments to %s", d.name)
	}
	defer func() {
		if r := recover(); r != nil {
			ret, ok := r.(returned)
			if !ok {
				panic(r)
			}
			out = ret.v
		}
	}()
	return in.run(d.body, ms)
}

func (in *interp) callBlock(p *proc, args ...value) value {
	if p.sym != "" {
		if len(args) == 0 {
			fail("&:%s without an argument", p.sym)
		}
		return in.method(args[0], p.sym, args[1:], nil)
	}
	s := &scope{vars: map[string]value{}, parent: p.env}
	for i, n := range p.b.params {
		if i < len(args) {
			s.vars[n] = args[i]
		} else {
			s.vars[n] = nil
		}
	}
	if len(p.b.params) > 1 && len(args) == 1 {
		if a, ok := args[0].(*arr); ok {
			for i, n := range p.b.params {
				if i < len(a.v) {
					s.vars[n] = a.v[i]
				}
			}
		}
	}
	return in.run(p.b.body, s)
}

func (in *interp) eval(n node, s *scope) value {
	switch x := n.(type) {
	case *litN:
		return x.v
	case *strN:
		var b strings.Builder
		for _, p := range x.parts {
			b.WriteString(str(in.eval(p, s)))
		}
		return rstr(b.String())
	case *symN:
		return sym(x.s)
	case *regexN:
		return &regex{x.src}
	case *arrN:
		a := &arr{}
		for _, e := range x.elems {
			a.v = append(a.v, in.eval(e, s))
		}
		return a
	case *hashN:
		h := &hash{}
		for _, p := range x.pairs {
			h.set(in.eval(p.k, s), in.eval(p.v, s))
		}
		return h
	case *constN:
		return in.constant(strings.Join(x.path, "::"))
	case *assignN:
		v := in.eval(x.v, s)
		s.assign(x.name, v)
		return v
	case *ifN:
		cond := in.eval(x.cond, s)
		if g, ok := cond.(*guardV); ok {
			// File.exist? in a flight block is checked when the step runs
			f := in.flight()
			f.guarded(in, s, g, false, x.then)
			if len(x.els) > 0 {
				f.guarded(in, s, g, true, x.els)
			}
			return nil
		}
		if truthy(cond) {
			return in.run(x.then, s)
		}
		return in.run(x.els, s)
	case *notN:
		v := in.eval(x.x, s)
		if g, ok := v.(*guardV); ok {
			return &guardV{path: g.path, negate: !g.negate}
		}
		return rbool(!truthy(v))
	case *negN:
		switch v := in.eval(x.x, s).(type) {
		case rint:
			return -v
		case rfloat:
			return -v
		}
		fail("negation")
	case *binN:
		switch x.op {
		case "&&":
			l := in.eval(x.l, s)
			if !truthy(l) {
				return l
			}
			return in.eval(x.r, s)
		case "||":
			l := in.eval(x.l, s)
			if truthy(l) {
				return l
			}
			return in.eval(x.r, s)
		}
		return binop(x.op, in.eval(x.l, s), in.eval(x.r, s))
	case *indexN:
		recv := in.eval(x.recv, s)
		var args []value
		for _, a := range x.args {
			args = append(args, in.eval(a, s))
		}
		return in.index(recv, args)
	case *callN:
		return in.evalCall(x, s)
	case *caseN:
		var subj value
		if x.subject != nil {
			subj = in.eval(x.subject, s)
		}
		for _, w := range x.whens {
			for _, p := range w.pats {
				pv := in.eval(p, s)
				if (x.subject == nil && truthy(pv)) || (x.subject != nil && caseEq(pv, subj)) {
					return in.run(w.body, s)
				}
			}
		}
		return in.run(x.els, s)
	case *returnN:
		var v value
		if x.v != nil {
			v = in.eval(x.v, s)
		}
		panic(returned{v})
	case *setN:
		v := in.eval(x.v, s)
		in.set(x.target, v, s)
		return v
	case *rescueN:
		// brw has no Ruby exceptions; what would raise is refused instead.
		return in.eval(x.x, s)
	case *beginN:
		if x.rescues {
			fail("begin with rescue")
		}
		defer in.run(x.ensure, s)
		return in.run(x.body, s)
	case *defN:
		for _, d := range slices.Backward(in.ctx) {
			if df, ok := d.(definer); ok {
				df.define(x)
				return sym(x.name)
			}
		}
		fail("def %s", x.name)
	}
	fail("node %T", n)
	return nil
}

// definer takes method definitions, as a formula class body does.
type definer interface{ define(d *defN) }

// setter handles assignments a DSL owns, such as ENV["X"] = v.
type setter interface {
	set(in *interp, target node, v value, s *scope) bool
}

func (in *interp) set(target node, v value, s *scope) {
	for _, d := range slices.Backward(in.ctx) {
		if st, ok := d.(setter); ok && st.set(in, target, v, s) {
			return
		}
	}
	if ix, ok := target.(*indexN); ok && len(ix.args) == 1 {
		switch recv := in.eval(ix.recv, s).(type) {
		case *hash:
			recv.set(in.eval(ix.args[0], s), v)
			return
		case *arr:
			if i, ok := in.eval(ix.args[0], s).(rint); ok && int(i) < len(recv.v) && i >= 0 {
				recv.v[i] = v
				return
			}
		}
	}
	fail("assignment")
}

// caseEq is Ruby's ===, as case/when uses it.
func caseEq(pat, v value) bool {
	switch p := pat.(type) {
	case *regex:
		return compile(p).MatchString(str(v))
	case *rangeV:
		i, ok := v.(rint)
		lo, hi := p.lo, p.hi
		return ok && int64(i) >= lo && (int64(i) < hi || (!p.exclusive && int64(i) == hi))
	}
	return eq(pat, v)
}

func (in *interp) constant(name string) value {
	switch name {
	case "MacOS", "OS", "Hardware::CPU", "ENV", "Pathname", "File", "Dir", "Formula", "Etc", "Process", "FileUtils", "Utils":
		return special(name)
	case "HOMEBREW_PREFIX":
		return rstr("$HOMEBREW_PREFIX")
	case "Language::Python::Virtualenv", "Language::Python::Shebang", "Language::Node", "Language::Perl::Shebang",
		"Language::Php::Shebang":
		// only ever included; their methods are refused where a formula calls them
		return special(name)
	case "HOMEBREW_TEMP":
		if t, ok := lookupEnv("HOMEBREW_TEMP"); ok && t != "" {
			return rstr(t)
		}
		return rstr("/private/tmp") // brew's default on macOS
	}
	fail("constant %s", name)
	return nil
}

func (in *interp) evalCall(c *callN, s *scope) value {
	if c.recv == nil {
		if c.bare {
			if v, ok := s.lookup(c.name); ok {
				return v
			}
		}
	}
	var recv value
	if c.recv != nil {
		recv = in.eval(c.recv, s)
		if c.safe && recv == nil {
			return nil
		}
	}
	var args []value
	var kw *hash
	var blk *proc
	if c.block != nil {
		blk = &proc{b: c.block, env: s}
	}
	for i, a := range c.args {
		switch x := a.(type) {
		case *symProcN:
			blk = &proc{sym: x.name}
			continue
		case *splatN:
			switch v := in.eval(x.x, s).(type) {
			case *arr:
				args = append(args, v.v...)
			case nil:
			default:
				args = append(args, v)
			}
			continue
		}
		v := in.eval(a, s)
		if h, ok := v.(*hash); ok && i == len(c.args)-1 {
			if _, isHashLit := a.(*hashN); isHashLit {
				kw = h
				continue
			}
		}
		args = append(args, v)
	}
	if c.recv == nil {
		for _, v := range slices.Backward(in.ctx) {
			if v, ok := v.call(in, s, c, args, kw); ok {
				return v
			}
		}
		fail("method %s", c.name)
	}
	if kw != nil {
		args = append(args, kw)
	}
	return in.method(recv, c.name, args, blk)
}

func binop(op string, l, r value) value {
	switch op {
	case "==":
		return rbool(eq(l, r))
	case "!=":
		return rbool(!eq(l, r))
	case "..", "...":
		a, ok1 := l.(rint)
		b, ok2 := r.(rint)
		if !ok1 || !ok2 {
			fail("range")
		}
		return &rangeV{int64(a), int64(b), op == "..."}
	}
	switch a := l.(type) {
	case rstr, ver:
		switch op {
		case "+":
			return rstr(str(a) + str(r))
		case "/": // Pathname#/
			return rstr(path.Join(str(a), str(r)))
		}
	case rint:
		b, ok := r.(rint)
		if !ok {
			fail("int %s %T", op, r)
		}
		return rbool(cmpOp(op, cmpInt(int64(a), int64(b))))
	case *arr:
		if op == "<<" {
			a.v = append(a.v, r)
			return a
		}
	case *macVer:
		b := toMacVer(r)
		return rbool(cmpOp(op, compareMac(a.v, b.v)))
	}
	fail("operator %s on %T", op, l)
	return nil
}

func cmpInt(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func cmpOp(op string, c int) bool {
	switch op {
	case "<":
		return c < 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	case ">=":
		return c >= 0
	case "==":
		return c == 0
	}
	fail("cmp %s", op)
	return false
}

// index is recv[args], which while a formula installs can read the real
// files and environment.
func (in *interp) index(recv value, args []value) value {
	if x, ok := recv.(special); ok && len(args) == 1 {
		switch {
		case x == "Formula":
			root := PrefixPlaceholder
			if in.build != nil {
				root = in.build.env.Prefix
			}
			return formulaRef{name: str(args[0]), root: root}
		case x == "Dir" && in.build != nil:
			return in.build.glob(str(args[0]))
		case x == "ENV" && in.build != nil:
			if v, ok := in.build.env.Env[str(args[0])]; ok {
				return rstr(v)
			}
			if v, ok := lookupEnv(str(args[0])); ok {
				return rstr(v)
			}
			return nil
		}
	}
	return index(recv, args)
}

func index(recv value, args []value) value {
	switch x := recv.(type) {
	case special:
		if x == "Dir" {
			// Dir[] globs staged files when brew loads the cask; none exist before install
			return &arr{}
		}
	case *arr:
		if len(args) == 1 {
			if r, ok := args[0].(*rangeV); ok {
				lo, hi := r.bounds(len(x.v))
				if lo > len(x.v) {
					return nil
				}
				return &arr{append([]value{}, x.v[lo:max(lo, hi)]...)}
			}
			if i, ok := args[0].(rint); ok {
				if i < 0 {
					i += rint(len(x.v))
				}
				if i < 0 || int(i) >= len(x.v) {
					return nil
				}
				return x.v[i]
			}
		}
	case rstr, ver:
		s := str(x)
		if len(args) == 1 {
			switch a := args[0].(type) {
			case rint:
				r := []rune(s)
				if a < 0 {
					a += rint(len(r))
				}
				if a < 0 || int(a) >= len(r) {
					return nil
				}
				return rstr(r[a])
			case *rangeV:
				r := []rune(s)
				lo, hi := a.bounds(len(r))
				if lo > len(r) {
					return nil
				}
				return rstr(r[lo:max(lo, hi)])
			}
		}
		if len(args) == 2 {
			if a, ok := args[0].(rint); ok {
				b, _ := args[1].(rint)
				r := []rune(s)
				if a < 0 || b < 0 || int(a) > len(r) {
					return nil
				}
				return rstr(r[a:min(int(a+b), len(r))])
			}
		}
	}
	fail("index %T", recv)
	return nil
}

type rangeV struct {
	lo, hi    int64
	exclusive bool
}

func (r *rangeV) bounds(n int) (int, int) {
	lo, hi := int(r.lo), int(r.hi)
	if lo < 0 {
		lo += n
	}
	if hi < 0 {
		hi += n
	}
	if !r.exclusive {
		hi++
	}
	return max(0, lo), min(max(hi, 0), n)
}

var reCache sync.Map // Ruby source to *regexp.Regexp

// compile turns a Ruby regex into Go's; the syntax casks use is common to both.
func compile(r *regex) *regexp.Regexp {
	if re, ok := reCache.Load(r.src); ok {
		return re.(*regexp.Regexp)
	}
	src := strings.ReplaceAll(r.src, `\h`, `[0-9a-fA-F]`)
	src = strings.ReplaceAll(src, `\Z`, `\n?\z`)
	re, err := regexp.Compile("(?m)" + src) // Ruby's ^ and $ always match at lines
	if err != nil {
		fail("regex %s", r.src)
	}
	reCache.Store(r.src, re)
	return re
}

func toRegexOrString(v value) (*regexp.Regexp, string, bool) {
	if r, ok := v.(*regex); ok {
		return compile(r), "", true
	}
	return nil, str(v), false
}

// rubySub converts a Ruby replacement (\1, \0) to Go's ($1).
func rubySub(s string) string {
	s = strings.ReplaceAll(s, "$", "$$")
	return rubyGroupRef.ReplaceAllString(s, "${$1}")
}

func (in *interp) method(recv value, name string, args []value, blk *proc) value {
	if in.build != nil {
		if s, ok := recv.(rstr); ok {
			if v, ok := in.build.pathMethod(in, string(s), name, args, blk); ok {
				return v
			}
		}
	}
	switch x := recv.(type) {
	case caskRef:
		if name == "url" {
			return x.b.url
		}
	case ver:
		if v, ok := versionMethod(x, name, args); ok {
			return v
		}
		return in.method(rstr(x), name, args, blk)
	case tmplV:
		switch name {
		case "major", "major_minor", "minor", "patch", "csv", "before_comma", "after_comma":
			return rstr("{{version." + name + "}}")
		}
	case rstr:
		return strMethod(string(x), name, args)
	case *arr:
		return in.arrMethod(x, name, args, blk)
	case *buffer:
		return bufferMethod(x, name, args)
	case resourceRef:
		if in.build != nil {
			return in.build.resourceMethod(in, x, name, args, blk)
		}
	case special:
		return in.specialMethod(x, name, args)
	case formulaRef:
		return x.method(name)
	case passwd:
		if name == "name" {
			return rstr(x)
		}
	}
	fail("method %s on %T", name, recv)
	return nil
}

var (
	majorMinorPatch = regexp.MustCompile(`^([^.,:]+)(?:.([^.,:]+)(?:.([^.,:]+))?)?`)
	dividers        = regexp.MustCompile(`[.\-_]`)
	rubyGroupRef    = regexp.MustCompile(`\\(\d)`)
)

func versionMethod(v ver, name string, args []value) (value, bool) {
	s := string(v)
	wrap := func(f func() string) value {
		if s == "" || s == "latest" {
			return v
		}
		return ver(f())
	}
	part := func(i int) string {
		m := majorMinorPatch.FindStringSubmatch(s)
		if m == nil {
			return ""
		}
		return m[i]
	}
	join := func(ps ...string) string {
		var out []string
		for _, p := range ps {
			if p != "" {
				out = append(out, p)
			}
		}
		return strings.Join(out, ".")
	}
	divs := map[string]string{"dots": ".", "hyphens": "-", "underscores": "_"}
	switch name {
	case "major":
		return wrap(func() string { return part(1) }), true
	case "minor":
		return wrap(func() string { return part(2) }), true
	case "patch":
		return wrap(func() string { return part(3) }), true
	case "major_minor":
		return wrap(func() string { return join(part(1), part(2)) }), true
	case "major_minor_patch":
		return wrap(func() string { return join(part(1), part(2), part(3)) }), true
	case "minor_patch":
		return wrap(func() string { return join(part(2), part(3)) }), true
	case "csv":
		a := &arr{}
		for p := range strings.SplitSeq(s, ",") {
			a.v = append(a.v, ver(p))
		}
		return a, true
	case "before_comma":
		return wrap(func() string { return strings.SplitN(s, ",", 2)[0] }), true
	case "after_comma":
		return wrap(func() string {
			p := strings.SplitN(s, ",", 2)
			if len(p) < 2 {
				return ""
			}
			return p[1]
		}), true
	case "no_dividers":
		return wrap(func() string { return dividers.ReplaceAllString(s, "") }), true
	case "chomp":
		return wrap(func() string {
			if len(args) > 0 {
				return strings.TrimSuffix(s, str(args[0]))
			}
			return strings.TrimRight(s, "\n")
		}), true
	case "latest?":
		return rbool(s == "latest"), true
	case "to_s", "to_str":
		return rstr(s), true
	}
	if strings.HasPrefix(name, "no_") {
		if d, ok := divs[name[3:]]; ok {
			return wrap(func() string { return strings.ReplaceAll(s, d, "") }), true
		}
	}
	if l, r, ok := strings.Cut(name, "_to_"); ok {
		if dl, ok1 := divs[l]; ok1 {
			if dr, ok2 := divs[r]; ok2 {
				return wrap(func() string { return strings.ReplaceAll(s, dl, dr) }), true
			}
		}
	}
	return nil, false
}

func strMethod(s string, name string, args []value) value {
	switch name {
	case "to_s":
		return rstr(s)
	case "downcase":
		return rstr(strings.ToLower(s))
	case "length", "size":
		return rint(len([]rune(s)))
	case "present?":
		return rbool(strings.TrimSpace(s) != "")
	case "dirname": // Pathname#dirname
		return rstr(path.Dir(s))
	}
	if len(args) == 0 {
		fail("String#%s without an argument", name)
	}
	switch name {
	case "delete_prefix":
		return rstr(strings.TrimPrefix(s, str(args[0])))
	case "tr":
		if len(args) < 2 {
			fail("String#tr needs two arguments")
		}
		from, to := []rune(expandTr(str(args[0]))), []rune(expandTr(str(args[1])))
		var b strings.Builder
		for _, r := range s {
			i := strings.IndexRune(string(from), r)
			if i < 0 {
				b.WriteRune(r)
				continue
			}
			ri := len([]rune(string(from)[:i]))
			if ri >= len(to) {
				ri = len(to) - 1
			}
			if ri >= 0 {
				b.WriteRune(to[ri])
			}
		}
		return rstr(b.String())
	case "gsub", "sub":
		if len(args) < 2 {
			fail("%s with a block", name)
		}
		re, lit, isRe := toRegexOrString(args[0])
		repl := str(args[1])
		if !isRe {
			if name == "sub" {
				return rstr(strings.Replace(s, lit, repl, 1))
			}
			return rstr(strings.ReplaceAll(s, lit, repl))
		}
		if name == "sub" {
			loc := re.FindStringSubmatchIndex(s)
			if loc == nil {
				return rstr(s)
			}
			var dst []byte
			dst = re.ExpandString(dst, rubySub(repl), s, loc)
			return rstr(s[:loc[0]] + string(dst) + s[loc[1]:])
		}
		return rstr(re.ReplaceAllString(s, rubySub(repl)))
	case "split":
		if len(args) != 1 {
			fail("String#split with a limit")
		}
		if _, ok := args[0].(*regex); ok {
			fail("String#split on a regex")
		}
		parts := strings.Split(s, str(args[0]))
		for len(parts) > 0 && parts[len(parts)-1] == "" { // Ruby drops trailing empty fields
			parts = parts[:len(parts)-1]
		}
		return strs(parts)
	case "ljust", "rjust":
		w, ok := args[0].(rint)
		if !ok {
			fail("String#%s width %T", name, args[0])
		}
		width := int(w)
		pad := " "
		if len(args) > 1 {
			pad = str(args[1])
		}
		n := len([]rune(s))
		if n >= width || pad == "" {
			return rstr(s)
		}
		fill := string([]rune(strings.Repeat(pad, width))[:width-n])
		if name == "ljust" {
			return rstr(s + fill)
		}
		return rstr(fill + s)
	case "join": // Pathname#join
		parts := []string{s}
		for _, a := range args {
			parts = append(parts, str(a))
		}
		return rstr(path.Join(parts...))
	}
	fail("String#%s", name)
	return nil
}

func expandTr(s string) string {
	r := []rune(s)
	var b strings.Builder
	for i := 0; i < len(r); i++ {
		if i+2 < len(r) && r[i+1] == '-' {
			for c := r[i]; c <= r[i+2]; c++ {
				b.WriteRune(c)
			}
			i += 2
			continue
		}
		b.WriteRune(r[i])
	}
	return b.String()
}

func (in *interp) arrMethod(a *arr, name string, args []value, blk *proc) value {
	nth := map[string]int{"first": 0, "second": 1, "third": 2, "fourth": 3, "fifth": 4}
	if i, ok := nth[name]; ok && len(args) == 0 {
		if i < len(a.v) {
			return a.v[i]
		}
		return nil
	}
	switch name {
	case "last":
		if len(a.v) == 0 {
			return nil
		}
		return a.v[len(a.v)-1]
	case "join":
		sep := ""
		if len(args) > 0 {
			sep = str(args[0])
		}
		parts := make([]string, len(a.v))
		for i, e := range a.v {
			parts[i] = str(e)
		}
		return rstr(strings.Join(parts, sep))
	case "map", "collect":
		if blk == nil {
			fail("Array#%s without a block", name)
		}
		out := &arr{}
		for _, e := range a.v {
			out.v = append(out.v, in.callBlock(blk, e))
		}
		return out
	case "each":
		if blk == nil {
			fail("Array#each without a block")
		}
		for _, e := range a.v {
			in.callBlock(blk, e)
		}
		return a
	case "each_with_index":
		if blk != nil {
			fail("Array#each_with_index with a block")
		}
		pairs := &arr{}
		for i, e := range a.v {
			pairs.v = append(pairs.v, &arr{[]value{e, rint(i)}})
		}
		return pairs
	case "size", "length", "count":
		return rint(len(a.v))
	}
	fail("Array#%s", name)
	return nil
}

// macVer is a macOS version for comparisons such as MacOS.version >= :big_sur.
type macVer struct{ v string }

var macSymbols = map[string]string{
	"golden_gate": "27", "tahoe": "26", "sequoia": "15", "sonoma": "14", "ventura": "13", "monterey": "12",
	"big_sur": "11", "catalina": "10.15", "mojave": "10.14", "high_sierra": "10.13", "sierra": "10.12",
	"el_capitan": "10.11",
}

func toMacVer(v value) *macVer {
	switch x := v.(type) {
	case *macVer:
		return x
	case sym:
		if s, ok := macSymbols[string(x)]; ok {
			return &macVer{s}
		}
	case rstr:
		return &macVer{string(x)}
	}
	fail("macOS version %v", v)
	return nil
}

func compareMac(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < max(len(pa), len(pb)); i++ {
		var x, y int64
		if i < len(pa) {
			x, _ = strconv.ParseInt(pa[i], 10, 64)
		}
		if i < len(pb) {
			y, _ = strconv.ParseInt(pb[i], 10, 64)
		}
		if c := cmpInt(x, y); c != 0 {
			return c
		}
	}
	return 0
}

func (in *interp) specialMethod(x special, name string, args []value) value {
	if b := in.build; b != nil {
		if v, ok := b.classMethod(x, name, args); ok {
			return v
		}
	}
	if f := in.flight(); f != nil && x == "File" && name != "expand_path" {
		return f.file(in, nil, name, args)
	}
	arg := func() string {
		if len(args) == 0 {
			fail("%s.%s without an argument", x, name)
		}
		return str(args[0])
	}
	switch string(x) + "." + name {
	case "MacOS.version":
		return &macVer{in.plat.macos}
	case "Hardware::CPU.intel?":
		return rbool(in.plat.real().arch == "intel")
	case "Hardware::CPU.arm?":
		return rbool(in.plat.real().arch == "arm")
	case "Hardware::CPU.is_64_bit?":
		return rbool(true)
	case "Hardware::CPU.is_32_bit?":
		return rbool(false)
	case "OS.mac?":
		return rbool(in.plat.real().os == "macos")
	case "OS.linux?":
		return rbool(in.plat.real().os == "linux")
	case "ENV.fetch":
		if v, ok := lookupEnv(arg()); ok {
			return rstr(v)
		}
		if len(args) > 1 {
			return args[1]
		}
		fail("ENV.fetch of unset %s", str(args[0]))
	case "Pathname.new":
		return rstr(arg())
	case "Dir.home":
		return rstr("/$HOME")
	case "Process.uid":
		return rint(0) // only ever passed to Etc.getpwuid
	case "Etc.getpwuid":
		return passwd(lookupUser())
	case "File.expand_path":
		return rstr(expandHome(arg()))
	}
	fail("%s.%s", x, name)
	return nil
}
