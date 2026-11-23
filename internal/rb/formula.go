package rb

import (
	"encoding/json"
	"fmt"
	"os/user"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/jigsmiths/brw/internal/version"
)

type Formula struct {
	b *formulaB
}

const (
	PrefixPlaceholder = "$HOMEBREW_PREFIX"
	CellarPlaceholder = "$HOMEBREW_CELLAR"
)

var hostPlatform = func() *platform {
	p := &platform{os: "macos", arch: "intel"}
	if runtime.GOOS == "linux" {
		p.os = "linux"
	}
	if runtime.GOARCH == "arm64" {
		p.arch = "arm"
	}
	return p
}

var lookupUser = func() string {
	if u, err := user.Current(); err == nil {
		return u.Username
	}
	return ""
}

func LoadFormula(src []byte, name, tag string) (*Formula, error) {
	code, data, _ := strings.Cut(string(src), "\n__END__\n")
	toks, err := lex(code)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	prog, err := parse(toks)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	plat := newPlatform(tag)
	plat.host = hostPlatform()
	b, err := evalFormula(prog, name, plat)
	if err != nil {
		return nil, err
	}
	b.data = data
	return &Formula{b}, nil
}

// SkipClean are skip_clean paths relative to the prefix; a symbol such as :la keeps its colon.
func (f *Formula) SkipClean() []string { return f.b.skipClean }

func (f *Formula) PreserveRpath() bool { return f.b.preserveRpath }

func symListKeep(args []value) []string {
	var out []string
	for _, a := range args {
		switch x := a.(type) {
		case *arr:
			out = append(out, symListKeep(x.v)...)
		case sym:
			out = append(out, ":"+string(x))
		default:
			out = append(out, str(x))
		}
	}
	return out
}

// MarshalJSON writes brew's Formula#to_hash for what the file decides; the tap supplies the rest.
func (f *Formula) MarshalJSON() (b []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			u, ok := r.(unsupported)
			if !ok {
				panic(r)
			}
			err = fmt.Errorf("%s: %w", f.b.name, u)
		}
	}()
	return json.Marshal(f.b.hash())
}

type spec struct {
	url      string
	specs    *hash
	using    value
	checksum string
	version  string
	mirrors  []string
	deps     []dep
	reqs     []req
	patches  []value
	resource []*spec
	name     string
}

func (s *spec) detectedVersion() string {
	if s.version != "" {
		return s.version
	}
	return version.Detect(s.url, str(kwGet(s.specs, "tag")))
}

type dep struct {
	name   string
	tags   []string
	macos  bool
	bounds *hash
}

func (d dep) has(tag string) bool { return slices.Contains(d.tags, tag) }

type req struct {
	name, version string
	contexts      []string
}

type formulaB struct {
	name, class   string
	plat          *platform
	desc, home    value
	license       value
	stable, head  *spec
	hasHead       bool
	cur           *spec
	revision      int64
	scheme        int64
	compat        value
	bottle        *bottleB
	kegOnly       *hash
	options       []*hash
	conflicts     []string
	conflictWhy   []value
	linkOverwrite []string
	pourOnlyIf    value
	service       *serviceB
	postSteps     *stepsDSL
	skipClean     []string
	preserveRpath bool
	noAutobump    value
	skipLivecheck bool
	serviceBlk    *blockN
	serviceScope  *scope
	data          string

	deprecationDate, deprecationReason, depReplF, depReplC value
	disableDate, disableReason, disReplF, disReplC         value
	deprecated, disabled                                   bool
	deprecateArgs, disableArgs                             value

	defs      map[string]*defN
	classDefs map[string]*defN
}

var knownRequires = map[string]bool{
	"etc": true, "json": true, "fileutils": true, "open3": true, "utils/linkage": true,
	"language/node": true, "language/perl": true, "language/python": true, "language/php": true,
	"language/java": true, "language/go": true,
}

func evalFormula(prog []node, name string, plat *platform) (b *formulaB, err error) {
	defer func() {
		if r := recover(); r != nil {
			switch x := r.(type) {
			case unsupported:
				err = x
			case parseErr:
				err = unsupported{"parse " + x.msg}
			case returned:
				err = unsupported{"return outside a method"}
			default:
				panic(r)
			}
		}
	}()
	b = &formulaB{name: name, plat: plat, defs: map[string]*defN{}, classDefs: map[string]*defN{}}
	b.stable = &spec{specs: &hash{}}
	b.head = &spec{specs: &hash{}, version: "HEAD"}
	in := &interp{plat: plat}
	top := &scope{vars: map[string]value{}}
	for _, n := range prog {
		switch x := n.(type) {
		case *callN:
			if x.recv != nil || x.name != "require" || len(x.args) != 1 {
				fail("top-level %s", x.name)
			}
			if r := str(in.eval(x.args[0], top)); !knownRequires[r] {
				fail("require %q", r)
			}
		case *classN:
			if x.super != "Formula" {
				fail("class %s < %s", x.name, x.super)
			}
			if b.class != "" {
				fail("second class %s", x.name)
			}
			b.class = x.name
			in.ctx = append(in.ctx, b)
			in.run(x.body, &scope{vars: map[string]value{}, parent: top})
		default:
			fail("top-level statement")
		}
	}
	if b.class == "" {
		fail("no formula class")
	}
	if b.stable.url == "" {
		if !b.hasHead {
			fail("formula requires at least a URL")
		}
		b.stable = nil
	}
	return b, nil
}

func (b *formulaB) define(d *defN) {
	if d.self {
		b.classDefs[d.name] = d
	} else {
		b.defs[d.name] = d
	}
}

func (b *formulaB) inSpec(in *interp, s *scope, sp *spec, blk *blockN) {
	prev := b.cur
	b.cur = sp
	defer func() { b.cur = prev }()
	in.run(blk.body, &scope{vars: map[string]value{}, parent: s})
}

func (b *formulaB) version() string {
	if b.stable == nil {
		return ""
	}
	return b.stable.detectedVersion()
}

func (b *formulaB) pkgVersion() string {
	v := b.version()
	if b.revision > 0 {
		v += "_" + strconv.FormatInt(b.revision, 10)
	}
	return v
}

func (b *formulaB) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	arg := func(i int) value {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	if ok, isOn := onCondition(c.name, args, in.plat); isOn {
		if c.block == nil {
			fail("%s without block", c.name)
		}
		if ok {
			in.run(c.block.body, &scope{vars: map[string]value{}, parent: s})
		}
		return nil, true
	}
	if d, ok := b.classDefs[c.name]; ok {
		return in.callDef(d, args, kw, s), true
	}
	sp := b.cur
	if sp == nil {
		sp = b.stable
	}
	switch c.name {
	case "desc":
		b.desc = arg(0)
	case "homepage":
		b.home = arg(0)
	case "license":
		if kw != nil && len(args) == 0 {
			b.license = kw
		} else {
			b.license = arg(0)
		}
	case "url":
		if len(args) == 0 {
			return rstr(sp.url), true
		}
		sp.url = str(arg(0))
		sp.specs = &hash{}
		if kw != nil {
			for i, k := range kw.keys {
				if k == sym("using") {
					sp.using = kw.vals[i]
					continue
				}
				sp.specs.set(k, kw.vals[i])
			}
		}
	case "mirror":
		sp.mirrors = append(sp.mirrors, str(arg(0)))
	case "sha256":
		sp.checksum = strings.ToLower(str(arg(0)))
	case "version":
		if len(args) == 0 {
			return ver(b.version()), true
		}
		sp.version = str(arg(0))
	case "name":
		return rstr(b.name), true
	case "revision":
		b.revision = int64(arg(0).(rint))
	case "version_scheme":
		b.scheme = int64(arg(0).(rint))
	case "compatibility_version":
		b.compat = arg(0)
	case "stable":
		if c.block == nil {
			fail("stable without a block")
		}
		b.inSpec(in, s, b.stable, c.block)
	case "head":
		b.hasHead = true
		if len(args) > 0 {
			b.head.url = str(arg(0))
			b.head.specs = &hash{}
			if kw != nil {
				for i, k := range kw.keys {
					if k == sym("using") {
						b.head.using = kw.vals[i]
						continue
					}
					b.head.specs.set(k, kw.vals[i])
				}
			}
		}
		if c.block != nil {
			b.inSpec(in, s, b.head, c.block)
		}
	case "resource":
		r := &spec{specs: &hash{}, name: str(arg(0))}
		if c.block == nil {
			fail("resource without a block")
		}
		b.inSpec(in, s, r, c.block)
		sp.resource = append(sp.resource, r)
	case "patch":
		b.patch(in, s, sp, c, args)
	case "bottle":
		if c.block == nil {
			fail("bottle %v", arg(0))
		}
		if b.bottle == nil {
			b.bottle = &bottleB{}
		}
		in.ctx = append(in.ctx, b.bottle)
		in.run(c.block.body, &scope{vars: map[string]value{}, parent: s})
		in.ctx = in.ctx[:len(in.ctx)-1]
	case "depends_on":
		b.forSpecs(func(sp *spec) { b.dependsOn(sp, args, kw) })
	case "uses_from_macos":
		b.forSpecs(func(sp *spec) { b.usesFromMacOS(sp, args, kw) })
	case "define_method":
		name, ok := arg(0).(sym)
		if !ok || c.block == nil || len(args) != 1 {
			fail("define_method")
		}
		d := &defN{name: string(name), body: c.block.body}
		for _, p := range c.block.params {
			d.params = append(d.params, param{name: p})
		}
		b.defs[d.name] = d
	case "require":
		if len(args) != 1 || !knownRequires[str(arg(0))] {
			fail("require %v", arg(0))
		}
	case "option":
		o := &hash{}
		o.put("option", rstr("--"+str(arg(0))))
		desc := arg(1)
		if desc == nil {
			desc = rstr("")
		}
		o.put("description", desc)
		b.options = append(b.options, o)
	case "conflicts_with":
		for _, a := range args {
			b.conflicts = append(b.conflicts, str(a))
			b.conflictWhy = append(b.conflictWhy, kwGet(kw, "because"))
		}
	case "keg_only":
		reason := arg(0)
		h := &hash{}
		if s, ok := reason.(sym); ok {
			h.put("reason", rstr(":"+string(s)))
		} else {
			h.put("reason", rstr(str(reason)))
		}
		expl := arg(1)
		if expl == nil {
			expl = rstr("")
		}
		h.put("explanation", expl)
		b.kegOnly = h
	case "link_overwrite":
		for _, a := range args {
			b.linkOverwrite = append(b.linkOverwrite, str(a))
		}
	case "pour_bottle?":
		if v := kwGet(kw, "only_if"); v != nil {
			b.pourOnlyIf = rstr(str(v))
		} else {
			fail("pour_bottle? block")
		}
	case "service":
		if c.block == nil {
			fail("service without a block")
		}
		b.service = &serviceB{f: b}
		b.serviceBlk, b.serviceScope = c.block, s
		in.ctx = append(in.ctx, b.instance(), b.service)
		in.run(c.block.body, &scope{vars: map[string]value{}, parent: s})
		in.ctx = in.ctx[:len(in.ctx)-2]
	case "post_install_steps":
		if c.block == nil {
			fail("post_install_steps without a block")
		}
		b.postSteps = &stepsDSL{formula: true}
		in.ctx = append(in.ctx, b.postSteps)
		in.run(c.block.body, &scope{vars: map[string]value{}, parent: s})
		in.ctx = in.ctx[:len(in.ctx)-1]
	case "deprecate!", "disable!":
		b.lifecycle(c.name, kw)
	case "skip_clean":
		b.skipClean = append(b.skipClean, symListKeep(args)...)
	case "include":
		if len(args) != 1 {
			fail("include")
		}
	case "preserve_rpath":
		b.preserveRpath = true
	case "no_autobump!":
		because := kwGet(kw, "because")
		if because == nil {
			fail("no_autobump! without because")
		}
		b.noAutobump = because
	case "livecheck":
		if c.block != nil {
			for _, n := range c.block.body {
				if call, ok := n.(*callN); ok && call.recv == nil && call.name == "skip" {
					b.skipLivecheck = true
				}
			}
		}
	case "test", "autobump", "allow_network_access!", "deny_network_access!",
		"cxxstdlib_check", "fails_with", "needs", "env", "plist_options", "opoo", "pour_bottle":
	default:
		return nil, false
	}
	return nil, true
}

func (b *formulaB) patch(in *interp, s *scope, sp *spec, c *callN, args []value) {
	strip := "p1"
	data := false
	for _, a := range args {
		switch a {
		case sym("p0"), sym("p1"), sym("p2"):
			strip = string(a.(sym))
		case sym("DATA"):
			data = true
		default:
			if _, ok := a.(rstr); ok {
				fail("patch from a string")
			}
			fail("patch %v", a)
		}
	}
	p := &hash{}
	p.put("strip", rstr(strip))
	if data {
		p.put("data", rbool(true))
		sp.patches = append(sp.patches, p)
		return
	}
	if c.block == nil {
		fail("patch without a source")
	}
	r := &spec{specs: &hash{}}
	plat := in.plat
	in.plat = plat.real()
	b.inSpec(in, s, r, c.block)
	in.plat = plat
	p.put("url", rstr(r.url))
	p.put("sha256", rstr(r.checksum))
	sp.patches = append(sp.patches, p)
}

func (b *formulaB) lifecycle(name string, kw *hash) {
	date := kwGet(kw, "date")
	because := kwGet(kw, "because")
	rf, rc := kwGet(kw, "replacement_formula"), kwGet(kw, "replacement_cask")
	if r := kwGet(kw, "replacement"); r != nil {
		fail("lifecycle replacement:")
	}
	if date == nil {
		fail("%s without a date", name)
	}
	d := str(date)
	args := lifecycleArgs(rstr(d), because, rf, rc)
	if name == "deprecate!" {
		b.deprecateArgs = args
		b.deprecationDate = rstr(d)
		if d <= today() {
			b.deprecated, b.deprecationReason, b.depReplF, b.depReplC = true, because, rf, rc
		}
		return
	}
	b.disableArgs = args
	b.disableDate = rstr(d)
	if d > today() {
		if b.deprecationDate == nil {
			b.deprecated, b.deprecationReason, b.depReplF, b.depReplC = true, because, rf, rc
		}
		return
	}
	b.disabled, b.disableReason, b.disReplF, b.disReplC = true, because, rf, rc
}

var depTags = map[string]bool{"build": true, "test": true, "optional": true, "recommended": true, "no_linkage": true, "implicit": true}

func symList(v value) []string {
	var out []string
	switch x := v.(type) {
	case nil:
	case *arr:
		for _, e := range x.v {
			out = append(out, symList(e)...)
		}
	default:
		out = append(out, str(x))
	}
	return out
}

func (b *formulaB) dependsOn(sp *spec, args []value, kw *hash) {
	var name value
	var tags []string
	switch {
	case len(args) == 1 && kw == nil:
		name = args[0]
	case len(args) == 0 && kw != nil && len(kw.keys) == 1:
		name, tags = kw.keys[0], symList(kw.vals[0])
	default:
		fail("depends_on %v", args)
	}
	switch n := name.(type) {
	case rstr:
		for _, t := range tags {
			if !depTags[t] {
				fail("depends_on tag %s", t)
			}
		}
		sp.deps = append(sp.deps, dep{name: string(n), tags: tags})
		if sp == b.stable {
			b.depOption(dep{name: string(n), tags: tags})
		}
	case sym:
		sp.reqs = append(sp.reqs, b.requirement(string(n), tags))
	default:
		fail("depends_on %v", name)
	}
}

func (b *formulaB) depOption(d dep) {
	short := d.name[strings.LastIndex(d.name, "/")+1:]
	var flag, desc string
	switch {
	case d.has("optional"):
		flag, desc = "--with-"+short, "Build with "+short+" support"
	case d.has("recommended"):
		flag, desc = "--without-"+short, "Build without "+short+" support"
	default:
		return
	}
	for _, o := range b.options {
		if v, _ := o.get(rstr("option")); str(v) == flag {
			return
		}
	}
	o := &hash{}
	o.put("option", rstr(flag))
	o.put("description", rstr(desc))
	b.options = append(b.options, o)
}

func (b *formulaB) forSpecs(f func(*spec)) {
	if b.cur != nil {
		f(b.cur)
		return
	}
	f(b.stable)
	f(b.head)
}

func (b *formulaB) requirement(name string, tags []string) req {
	r := req{name: name}
	var contexts []string
	for _, t := range tags {
		switch {
		case depTags[t]:
			contexts = append(contexts, t)
		case name == "xcode" && r.version == "" && strings.ContainsAny(t, "0123456789"):
			r.version = t
		case name == "arch" && r.version == "":
			r.version = t
		case (name == "macos" || name == "maximum_macos") && r.version == "":
			v, ok := macSymbols[t]
			if !ok {
				fail("depends_on macos: %s", t)
			}
			r.version = v
		default:
			fail("requirement %s tag %s", name, t)
		}
	}
	switch name {
	case "linux", "macos", "maximum_macos", "xcode", "arch":
	default:
		fail("requirement %s", name)
	}
	r.contexts = contexts
	return r
}

func (b *formulaB) usesFromMacOS(sp *spec, args []value, kw *hash) {
	d := dep{macos: true, bounds: &hash{}}
	switch {
	case len(args) == 1:
		d.name = str(args[0])
		if kw != nil {
			d.bounds = kw
		}
	case len(args) == 0 && kw != nil && len(kw.keys) >= 1:
		d.name = str(kw.keys[0])
		d.tags = symList(kw.vals[0])
		for i := 1; i < len(kw.keys); i++ {
			d.bounds.set(kw.keys[i], kw.vals[i])
		}
	default:
		fail("uses_from_macos %v", args)
	}
	sp.deps = append(sp.deps, d)
}
