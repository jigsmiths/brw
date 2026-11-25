// Package rb evaluates Homebrew's Ruby cask files without Ruby.
package rb

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
)

type platform struct {
	os    string
	arch  string
	macos string
	host  *platform
}

func (p *platform) real() *platform {
	if p.host != nil {
		return p.host
	}
	return p
}

var ErrUnsupported = errors.New("unsupported Ruby")

var (
	today     = func() string { return time.Now().Format(time.DateOnly) }
	lookupEnv = os.LookupEnv
)

// LoadCask evaluates a cask file for the platform tag; flight blocks become install steps.
func LoadCask(src []byte, tag string) (api.Cask, error) {
	c, err := evaluate(src, tag)
	if err != nil {
		return api.Cask{}, err
	}
	b, err := json.Marshal(c.hash(false))
	if err != nil {
		return api.Cask{}, err
	}
	var out api.Cask
	if err := json.Unmarshal(b, &out); err != nil {
		return api.Cask{}, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return out, nil
}

func evaluate(src []byte, tag string) (*caskB, error) {
	toks, err := lex(string(src))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	prog, err := parse(toks)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return evalCask(prog, newPlatform(tag))
}

func newPlatform(tag string) *platform {
	p := &platform{os: "macos", arch: "intel"}
	name, arm := strings.CutPrefix(tag, "arm64_")
	if arm {
		p.arch = "arm"
	}
	if strings.HasSuffix(tag, "_linux") {
		p.os = "linux"
		return p
	}
	p.macos = macSymbols[name]
	return p
}

type artifact struct {
	key    string
	args   []value
	target string
	hasTgt bool
	flight []value
}

type flightDSL struct{ st stepsDSL }

type guardV struct {
	path   string
	negate bool
}

func (d *flightDSL) file(in *interp, s *scope, name string, args []value) value {
	switch name {
	case "exist?", "file?", "directory?", "exists?":
		return &guardV{path: str(args[0])}
	case "write":
		kw := &hash{}
		kw.set(sym("overwrite"), rbool(true))
		d.st.call(in, s, &callN{name: "write_file"}, args[:2], kw)
	case "delete", "unlink":
		d.st.call(in, s, &callN{name: "remove"}, []value{&arr{args}}, nil)
	default:
		fail("File.%s", name)
	}
	return nil
}

func (d *flightDSL) guarded(in *interp, s *scope, g *guardV, negate bool, body []node) {
	cond := "if_exists"
	if negate != g.negate {
		cond = "unless_exists"
	}
	d.st.guardID++
	h := pathSpec(rstr(g.path), nil, nil, true)
	h.put("condition", rstr(cond))
	h.put("id", rstr(strconv.Itoa(d.st.guardID)))
	prev := d.st.guards
	d.st.guards = append(append([]value{}, d.st.guards...), h)
	in.run(body, s)
	d.st.guards = prev
}

func (d *flightDSL) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	st := &d.st
	switch c.name {
	case "system_command", "system_command!":
		run := &hash{}
		if kw != nil {
			for i, k := range kw.keys {
				switch str(k) {
				case "args", "sudo", "must_succeed", "print_stderr", "print_stdout", "env":
					run.set(k, kw.vals[i])
				default:
					fail("system_command option %s", str(k))
				}
			}
		}
		if _, ok := run.get(sym("must_succeed")); !ok {
			run.set(sym("must_succeed"), rbool(c.name == "system_command!"))
		}
		st.call(in, s, &callN{name: "run"}, args[:1], run)
		return nil, true
	case "system":
		if len(args) == 0 || kw != nil {
			fail("system without a command")
		}
		run := &hash{}
		run.set(sym("args"), &arr{args[1:]})
		run.set(sym("must_succeed"), rbool(false))
		st.call(in, s, &callN{name: "run"}, args[:1], run)
		return nil, true
	}
	return nil, false
}

func (in *interp) flight() *flightDSL {
	for _, v := range slices.Backward(in.ctx) {
		if f, ok := v.(*flightDSL); ok {
			return f
		}
	}
	return nil
}

type caskB struct {
	token     string
	names     []string
	desc      value
	homepage  value
	version   value
	sha256    value
	url       value
	urlSpecs  *hash
	archV     value
	osV       value
	langs     []langBlock
	langDef   *proc
	langEval  value
	langDone  bool
	autoUpd   value
	conflicts *hash
	depends   *hash
	container *hash
	custom    []string
	builtin   []builtinCaveat
	rosetta   bool
	artifacts []artifact
	rename    []value

	deprecated, disabled                   bool
	deprecationDate, disableDate           value
	deprecationReason, disableReason       value
	depReplF, depReplC, disReplF, disReplC value
}

type langBlock struct {
	codes []string
	p     *proc
}

type builtinCaveat struct {
	name string
	text string
}

func evalCask(prog []node, plat *platform) (c *caskB, err error) {
	defer func() {
		if r := recover(); r != nil {
			if u, ok := r.(unsupported); ok {
				err = u
				return
			}
			if pe, ok := r.(parseErr); ok {
				err = unsupported{"parse " + pe.msg}
				return
			}
			panic(r)
		}
	}()
	c = &caskB{}
	in := &interp{plat: plat}
	top := &scope{vars: map[string]value{}}
	for _, n := range prog {
		call, ok := n.(*callN)
		if !ok || call.name != "cask" || call.block == nil || len(call.args) != 1 {
			fail("top-level statement")
		}
		c.token = str(in.eval(call.args[0], top))
		in.ctx = append(in.ctx, c)
		in.run(call.block.body, &scope{vars: map[string]value{}, parent: top})
		c.language(in)
	}
	if c.token == "" {
		fail("no cask")
	}
	return c, nil
}

func kwGet(kw *hash, k string) value {
	if kw == nil {
		return nil
	}
	v, _ := kw.get(sym(k))
	return v
}

func (b *caskB) pick(arm, intel value, plat *platform) value {
	if plat.arch == "arm" {
		return arm
	}
	return intel
}

func onCondition(name string, args []value, plat *platform) (bool, bool) {
	switch name {
	case "on_arm":
		return plat.arch == "arm", true
	case "on_intel":
		return plat.arch == "intel", true
	case "on_macos":
		return plat.os == "macos", true
	case "on_linux":
		return plat.os == "linux", true
	}
	if !strings.HasPrefix(name, "on_") {
		return false, false
	}
	v, ok := macSymbols[name[3:]]
	if !ok {
		return false, false
	}
	if plat.os != "macos" {
		return false, true
	}
	c := compareMac(plat.macos, v)
	if len(args) == 0 {
		return c == 0, true
	}
	switch args[0] {
	case sym("or_older"):
		return c <= 0, true
	case sym("or_newer"):
		return c >= 0, true
	}
	fail("%s %v", name, args[0])
	return false, true
}

var relocated = map[string]string{
	"app": "/Applications", "app_image": "/$HOME/Applications", "suite": "/Applications",
	"artifact": "", "colorpicker": "/$HOME/Library/ColorPickers", "prefpane": "/$HOME/Library/PreferencePanes",
	"qlplugin": "/$HOME/Library/QuickLook", "mdimporter": "/$HOME/Library/Spotlight",
	"dictionary": "/$HOME/Library/Dictionaries", "font": "/$HOME/Library/Fonts", "service": "/$HOME/Library/Services",
	"input_method": "/$HOME/Library/Input Methods", "internet_plugin": "/$HOME/Library/Internet Plug-Ins",
	"keyboard_layout": "/Library/Keyboard Layouts", "audio_unit_plugin": "/$HOME/Library/Audio/Plug-Ins/Components",
	"vst_plugin": "/$HOME/Library/Audio/Plug-Ins/VST", "vst3_plugin": "/$HOME/Library/Audio/Plug-Ins/VST3",
	"screen_saver": "/$HOME/Library/Screen Savers", "binary": "$HOMEBREW_PREFIX/bin",
	"command_wrapper": "$HOMEBREW_PREFIX/bin", "manpage": "$HOMEBREW_PREFIX/share/man",
	"bash_completion": "$HOMEBREW_PREFIX/etc/bash_completion.d", "zsh_completion": "$HOMEBREW_PREFIX/share/zsh/site-functions",
	"fish_completion": "$HOMEBREW_PREFIX/share/fish/vendor_completions.d", "pwsh_completion": "$HOMEBREW_PREFIX/share/pwsh/completions",
}

var plainArtifacts = map[string]bool{
	"pkg": true, "installer": true, "stage_only": true, "generated_script": true,
	"generate_completions_from_executable": true, "uninstall": true, "zap": true,
}

var artifactOrder = func() map[string]int {
	groups := [][]string{
		{"preflight_steps"}, {"uninstall_preflight_steps"}, {"preflight"}, {"uninstall"}, {"generated_script"},
		{"installer"}, {"pkg"},
		{"app", "app_image", "suite", "artifact", "colorpicker", "prefpane", "qlplugin", "mdimporter", "dictionary",
			"font", "service", "input_method", "internet_plugin", "keyboard_layout", "audio_unit_plugin", "vst_plugin",
			"vst3_plugin", "screen_saver"},
		{"binary", "command_wrapper"}, {"manpage"},
		{"bash_completion", "zsh_completion", "fish_completion", "pwsh_completion"},
		{"generate_completions_from_executable"}, {"postflight_steps"}, {"uninstall_postflight_steps"},
		{"postflight"}, {"zap"},
	}
	m := map[string]int{}
	for i, g := range groups {
		for _, k := range g {
			m[k] = i
		}
	}
	m["stage_only"] = -1 //guard:allow a sort rank that puts stage_only before every group
	m["uninstall_preflight"] = m["preflight"]
	m["uninstall_postflight"] = m["postflight"]
	return m
}()

func basename(s string) string { return path.Base(s) }

func expandHome(t string) string {
	if t == "~" || strings.HasPrefix(t, "~/") {
		return "/$HOME" + t[1:]
	}
	return t
}

func resolveTarget(key, source, target string) string {
	if target == "" {
		target = basename(source)
	}
	switch key {
	case "manpage":
		m := manSection.FindStringSubmatch(source)
		sec := ""
		if m != nil {
			sec = m[1]
		}
		return relocated[key] + "/man" + sec + "/" + target
	case "bash_completion":
		return relocated[key] + "/" + strings.TrimSuffix(target, path.Ext(target))
	case "zsh_completion":
		if !strings.HasPrefix(target, "_") {
			target = "_" + strings.TrimSuffix(target, path.Ext(target))
		}
		return relocated[key] + "/" + target
	case "fish_completion":
		if !strings.HasSuffix(target, ".fish") {
			target = strings.TrimSuffix(target, path.Ext(target)) + ".fish"
		}
		return relocated[key] + "/" + target
	case "pwsh_completion":
		if !strings.HasPrefix(target, "_") || !strings.HasSuffix(target, ".ps1") {
			target = "_" + strings.TrimSuffix(target, path.Ext(target)) + ".ps1"
		}
		return relocated[key] + "/" + target
	case "app_image":
		return relocated[key] + "/" + target
	}
	if strings.HasPrefix(target, "/") || strings.HasPrefix(target, "$") || key == "artifact" {
		return expandHome(target)
	}
	if strings.HasPrefix(target, "~") {
		return expandHome(target)
	}
	return relocated[key] + "/" + target
}

func (b *caskB) addArtifact(key string, args []value, kw *hash) {
	all := append([]value{}, args...)
	switch key {
	case "zap":
		if len(args) == 1 && args[0] == sym("not_required") {
			all = nil
		}
	case "generate_completions_from_executable":
		opts := &hash{}
		opts.put("base_name", kwGet(kw, "base_name"))
		format := kwGet(kw, "shell_parameter_format")
		opts.put("shell_parameter_format", format)
		shells := kwGet(kw, "shells")
		if shells == nil {
			shells = strs([]string{"bash", "zsh", "fish"})
			if format == sym("cobra") || format == sym("typer") {
				shells = strs([]string{"bash", "zsh", "fish", "pwsh"})
			}
		}
		opts.put("shells", shells)
		kw = opts
	case "installer":
		if s := kwGet(kw, "script"); s != nil {
			if _, isHash := s.(*hash); !isHash {
				inner := &hash{}
				for i, k := range kw.keys {
					if str(k) == "script" {
						inner.put("executable", s)
					} else {
						inner.put(str(k), kw.vals[i])
					}
				}
				kw = &hash{}
				kw.put("script", inner)
			}
		}
	case "command_wrapper":
		opts := &hash{}
		if c := kwGet(kw, "content"); !blank(c) {
			opts.put("content", c)
		} else {
			opts.put("executable", rstr(str(kwGet(kw, "executable"))))
			if a := kwGet(kw, "args"); !blank(a) {
				l, ok := a.(*arr)
				if !ok {
					l = &arr{[]value{a}}
				}
				out := &arr{}
				for _, e := range l.v {
					out.v = append(out.v, rstr(str(e)))
				}
				opts.put("args", out)
			}
			if e, ok := kwGet(kw, "env").(*hash); ok && len(e.keys) > 0 {
				env := &hash{}
				for i, k := range e.keys {
					env.put(str(k), rstr(str(e.vals[i])))
				}
				opts.put("env", env)
			}
		}
		b.artifacts = append(b.artifacts, artifact{key: key, args: []value{rstr(str(args[0])), opts}, target: resolveTarget(key, str(args[0]), ""), hasTgt: true})
		return
	}
	if kw != nil && len(kw.keys) > 0 {
		all = append(all, kw)
	}
	a := artifact{key: key, args: compactBlank(all)}
	if _, ok := relocated[key]; ok {
		src := ""
		if len(args) > 0 {
			src = str(args[0])
		}
		tgt := ""
		if t := kwGet(kw, "target"); t != nil {
			tgt = str(t)
		}
		if key == "artifact" && tgt == "" {
			fail("artifact without target")
		}
		if key == "command_wrapper" {
			tgt = ""
		}
		a.target, a.hasTgt = resolveTarget(key, src, tgt), true
	}
	b.artifacts = append(b.artifacts, a)
}

func (b *caskB) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	plat := in.plat
	arg0 := func() value {
		if len(args) > 0 {
			return args[0]
		}
		return nil
	}
	if ok, isOn := onCondition(c.name, args, plat); isOn {
		if c.block == nil {
			fail("%s without block", c.name)
		}
		if ok {
			in.run(c.block.body, &scope{vars: map[string]value{}, parent: s})
		}
		return nil, true
	}
	switch c.name {
	case "version":
		if len(args) == 0 {
			return b.version, true
		}
		switch v := args[0].(type) {
		case rstr:
			b.version = ver(v)
		case sym:
			if v != "latest" {
				fail("version %v", v)
			}
			b.version = ver("latest")
		}
	case "sha256":
		if len(args) == 0 && kw == nil {
			return b.sha256, true
		}
		v := arg0()
		if v == nil {
			x86 := kwGet(kw, "x86_64")
			if x86 == nil {
				x86 = kwGet(kw, "intel")
			}
			if plat.os == "linux" {
				v = b.pick(kwGet(kw, "arm64_linux"), kwGet(kw, "x86_64_linux"), plat)
			} else {
				v = b.pick(kwGet(kw, "arm"), x86, plat)
			}
		}
		if v == sym("no_check") {
			v = rstr("no_check")
		} else if s, ok := v.(rstr); ok {
			v = rstr(strings.ToLower(string(s)))
		}
		b.sha256 = v
	case "url":
		if len(args) == 0 {
			return b.url, true
		}
		if c.block != nil {
			fail("url block")
		}
		b.url = rstr(str(args[0]))
		specs := &hash{}
		for _, k := range []string{"using", "tag", "branch", "revisions", "revision", "trust_cert", "cookies", "referer", "header", "user_agent", "data", "only_path"} {
			v := kwGet(kw, k)
			if v == nil {
				continue
			}
			switch k {
			case "header":
				if _, ok := v.(*arr); !ok {
					v = &arr{[]value{v}}
				}
				specs.put("headers", v)
				continue
			case "user_agent":
				if s, ok := v.(sym); ok {
					if s == "default" {
						continue
					}
					v = rstr(":" + string(s))
				}
			case "using":
				if s, ok := v.(sym); ok {
					v = rstr(s)
				}
			}
			specs.put(k, v)
		}
		if kw != nil {
			for _, k := range kw.keys {
				switch str(k) {
				case "verified", "using", "tag", "branch", "revisions", "revision", "trust_cert", "cookies", "referer", "header", "user_agent", "data", "only_path":
				default:
					fail("url option %s", str(k))
				}
			}
		}
		b.urlSpecs = specs
	case "name":
		if len(args) == 0 {
			return strs(b.names), true
		}
		for _, a := range args {
			b.names = append(b.names, str(a))
		}
	case "desc":
		if len(args) == 0 {
			return b.desc, true
		}
		b.desc = arg0()
	case "homepage":
		if len(args) == 0 {
			return b.homepage, true
		}
		b.homepage = arg0()
	case "arch":
		if kw == nil {
			return b.archV, true
		}
		b.archV = b.pick(kwGet(kw, "arm"), kwGet(kw, "intel"), plat)
	case "os":
		if kw == nil {
			return b.osV, true
		}
		if plat.os == "linux" {
			b.osV = kwGet(kw, "linux")
		} else {
			b.osV = kwGet(kw, "macos")
		}
	case "on_system_conditional":
		if plat.os == "linux" {
			return kwGet(kw, "linux"), true
		}
		return kwGet(kw, "macos"), true
	case "on_arch_conditional":
		return b.pick(kwGet(kw, "arm"), kwGet(kw, "intel"), plat), true
	case "language":
		if len(args) == 0 {
			return b.language(in), true
		}
		if c.block == nil {
			fail("language without block")
		}
		lb := langBlock{p: &proc{b: c.block, env: s}}
		for _, a := range args {
			lb.codes = append(lb.codes, str(a))
		}
		b.langs = append(b.langs, lb)
		if truthy(kwGet(kw, "default")) {
			b.langDef = lb.p
		}
	case "auto_updates":
		b.autoUpd = arg0()
	case "conflicts_with":
		if b.conflicts == nil {
			b.conflicts = &hash{}
		}
		for i, k := range kw.keys {
			v := kw.vals[i]
			a, ok := v.(*arr)
			if !ok {
				a = &arr{[]value{v}}
			}
			if old, ok := b.conflicts.get(k); ok {
				a = &arr{append(append([]value{}, old.(*arr).v...), a.v...)}
			}
			b.conflicts.set(k, a)
		}
	case "depends_on":
		b.dependsOn(args, kw)
	case "container":
		h := &hash{}
		if v := kwGet(kw, "nested"); v != nil {
			h.put("nested", v)
		}
		if v := kwGet(kw, "type"); v != nil {
			h.put("type", v)
		}
		b.container = h
	case "caveats":
		if c.block != nil {
			in.ctx = append(in.ctx, &caveatsDSL{b: b})
			v := in.callBlock(&proc{b: c.block, env: s})
			in.ctx = in.ctx[:len(in.ctx)-1]
			if v != nil && v != sym("__builtin") {
				b.custom = append(b.custom, caveatText(str(v)))
			}
		} else {
			for _, a := range args {
				b.custom = append(b.custom, caveatText(str(a)))
			}
		}
	case "deprecate!", "disable!":
		b.lifecycle(c.name, kw)
	case "rename":
		h := &hash{}
		h.put("from", args[0])
		h.put("to", args[1])
		b.rename = append(b.rename, h)
	case "livecheck", "no_autobump!":
	case "token":
		return rstr(b.token), true
	case "cask":
		return caskRef{b}, true
	case "appdir":
		return rstr("$APPDIR"), true
	case "caskroom_path":
		return rstr("$HOMEBREW_PREFIX/Caskroom/" + b.token), true
	case "Pathname":
		return rstr(str(args[0])), true
	case "staged_path":
		return rstr("$HOMEBREW_PREFIX/Caskroom/" + b.token + "/" + str(b.version)), true
	case "preflight_steps", "postflight_steps", "uninstall_preflight_steps", "uninstall_postflight_steps":
		if c.block == nil {
			fail("%s without block", c.name)
		}
		st := &stepsDSL{}
		in.ctx = append(in.ctx, st)
		in.run(c.block.body, &scope{vars: map[string]value{}, parent: s})
		in.ctx = in.ctx[:len(in.ctx)-1]
		h := &hash{}
		h.put("steps", &arr{st.steps})
		b.artifacts = append(b.artifacts, artifact{key: c.name, args: []value{h}})
	case "preflight", "postflight", "uninstall_preflight", "uninstall_postflight":
		if c.block == nil {
			fail("%s without block", c.name)
		}
		fl := &flightDSL{}
		in.ctx = append(in.ctx, fl)
		in.run(c.block.body, &scope{vars: map[string]value{}, parent: s})
		in.ctx = in.ctx[:len(in.ctx)-1]
		b.artifacts = append(b.artifacts, artifact{key: c.name, flight: fl.st.steps})
	default:
		if _, ok := relocated[c.name]; ok || plainArtifacts[c.name] {
			if c.block != nil {
				fail("block on %s", c.name)
			}
			b.addArtifact(c.name, args, kw)
			if (c.name == "uninstall" || c.name == "zap") && kwGet(kw, "kext") != nil {
				(&caveatsDSL{b: b}).call(in, s, &callN{name: "kext"}, nil, nil)
			}
			return nil, true
		}
		return nil, false
	}
	return nil, true
}

var (
	trailingSpace = regexp.MustCompile(`\s*\z`)
	manSection    = regexp.MustCompile(`\.([1-8nl])(?:\.gz)?$`)
	macComparison = regexp.MustCompile(`^\s*(<|>|[=<>]=)\s*:?(\S+)\s*$`)
)

func caveatText(s string) string {
	return trailingSpace.ReplaceAllString(s, "") + "\n"
}

func (b *caskB) language(in *interp) value {
	if b.langDone {
		return b.langEval
	}
	if len(b.langs) == 0 {
		return nil
	}
	if b.langDef == nil {
		fail("no default language")
	}
	b.langDone = true
	b.langEval = in.callBlock(b.langDef)
	return b.langEval
}

func (b *caskB) dependsOn(args []value, kw *hash) {
	if b.depends == nil {
		b.depends = &hash{}
	}
	if len(args) > 0 {
		if kw == nil {
			kw = &hash{}
		}
		switch args[0] {
		case sym("macos"):
			kw.set(sym("macos"), sym("any"))
		case sym("linux"):
			kw.set(sym("linux"), sym("any"))
		default:
			fail("depends_on %v", args[0])
		}
	}
	for i, k := range kw.keys {
		v := kw.vals[i]
		switch str(k) {
		case "formula", "cask":
			a, ok := v.(*arr)
			if !ok {
				a = &arr{[]value{v}}
			}
			if old, ok := b.depends.get(rstr(str(k))); ok {
				a = &arr{append(append([]value{}, old.(*arr).v...), a.v...)}
			}
			b.depends.put(str(k), a)
		case "macos", "maximum_macos":
			cmp := ">="
			if str(k) == "maximum_macos" {
				cmp = "<="
			}
			req := macReq(v, cmp)
			if v == sym("any") {
				if old, ok := b.depends.get(rstr("macos")); ok && len(old.(*hash).keys) > 0 {
					continue
				}
			}
			b.depends.put(str(k), req)
		case "arch":
			a, ok := v.(*arr)
			if !ok {
				a = &arr{[]value{v}}
			}
			out := &arr{}
			if old, ok := b.depends.get(rstr("arch")); ok {
				out.v = append(out.v, old.(*arr).v...)
			}
			for _, e := range a.v {
				h := &hash{}
				switch strings.ReplaceAll(strings.ToLower(str(e)), "-", "_") {
				case "intel", "x86_64":
					h.put("type", rstr("intel"))
				case "arm64":
					h.put("type", rstr("arm"))
				default:
					fail("arch %v", e)
				}
				h.put("bits", rint(64))
				out.v = append(out.v, h)
			}
			b.depends.put("arch", out)
		case "linux":
			b.depends.put("linux", &hash{})
		default:
			fail("depends_on %s", str(k))
		}
	}
}

func macReq(v value, cmp string) *hash {
	h := &hash{}
	vers := func(syms []value) *arr {
		a := &arr{}
		for _, s := range syms {
			mv, ok := macSymbols[str(s)]
			if !ok {
				continue
			}
			a.v = append(a.v, rstr(mv))
		}
		return a
	}
	switch x := v.(type) {
	case sym:
		if x == "any" {
			return h
		}
		if _, ok := macSymbols[string(x)]; !ok {
			fail("macos %s", x)
		}
		h.put(cmp, vers([]value{x}))
	case *arr:
		if len(x.v) == 1 {
			return macReq(x.v[0], cmp)
		}
		h.put("==", vers(x.v))
	case rstr:
		s := string(x)
		m := macComparison.FindStringSubmatch(s)
		if m == nil {
			fail("macos %q", s)
		}
		vv := m[2]
		if strings.Contains(s, ":") {
			vv = macSymbols[vv]
		}
		h.put(m[1], strs([]string{vv}))
	default:
		fail("macos %T", v)
	}
	return h
}

func (b *caskB) lifecycle(name string, kw *hash) {
	date := str(kwGet(kw, "date"))
	because := kwGet(kw, "because")
	if name == "disable!" && because == sym("unsigned") {
		because = sym("fails_gatekeeper_check")
	}
	rf, rc := kwGet(kw, "replacement_formula"), kwGet(kw, "replacement_cask")
	if r := kwGet(kw, "replacement"); r != nil {
		fail("lifecycle replacement:")
	}
	now := today()
	if name == "deprecate!" {
		b.deprecationDate = rstr(date)
		if date <= now {
			b.deprecated, b.deprecationReason, b.depReplF, b.depReplC = true, because, rf, rc
		}
		return
	}
	b.disableDate = rstr(date)
	if date > now {
		if b.deprecationDate == nil {
			b.deprecated, b.deprecationReason, b.depReplF, b.depReplC = true, because, rf, rc
		}
		return
	}
	b.disabled, b.disableReason, b.disReplF, b.disReplC = true, because, rf, rc
}

type caveatsDSL struct{ b *caskB }

var caveatTemplates = map[string]string{
	"kext":                      "{cask} requires a kernel extension to work.\nIf the installation fails, retry after you enable it in:\n  System Settings → Privacy & Security\n\nFor more information, refer to vendor documentation or this Apple Technical Note:\n  https://developer.apple.com/library/content/technotes/tn2459/_index.html\n",
	"path_environment_variable": "To use {cask}, you may need to add the {0} directory\nto your PATH environment variable, e.g. (for Bash shell):\n  export PATH={0}:\"$PATH\"\n",
	"zsh_path_helper":           "To use {cask}, zsh users may need to add the following line to their\n~/.zprofile. (Among other effects, {0} will be added to the\nPATH environment variable):\n  eval `/usr/libexec/path_helper -s`\n",
	"files_in_usr_local":        "",
	"logout":                    "You must log out and log back in for the installation of {cask} to take effect.\n",
	"reboot":                    "You must reboot for the installation of {cask} to take effect.\n",
	"license":                   "Installing {cask} means you have AGREED to the license at:\n  {0}\n",
	"free_license":              "The vendor offers a free license for {cask} at:\n  {0}\n",
	"requires_rosetta":          "",
}

func (d *caveatsDSL) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	b := d.b
	text, ok := caveatTemplates[c.name]
	switch c.name {
	case "requires_rosetta":
		b.rosetta = true
		return sym("__builtin"), true
	case "files_in_usr_local":
		return sym("__builtin"), true
	case "kext":
		if in.plat.os != "macos" || compareMac(in.plat.macos, "14") < 0 {
			return sym("__builtin"), true
		}
	case "depends_on_java":
		v := value(sym("any"))
		if len(args) > 0 {
			v = args[0]
		}
		jv := str(v)
		switch {
		case v == sym("any"):
			text = "{cask} requires Java. You can install the latest version with:\n  brw install --cask temurin\n"
		case strings.Contains(jv, "+"):
			text = "{cask} requires Java " + jv + ". You can install the latest version with:\n  brw install --cask temurin\n"
		default:
			text = "{cask} requires Java " + jv + ". You can install it with:\n  brw install --cask temurin@" + jv + "\n"
		}
		ok = true
	case "unsigned_accessibility":
		if in.plat.os != "macos" {
			return sym("__builtin"), true
		}
		access := "Accessibility"
		if len(args) > 0 && args[0] != nil {
			access = str(args[0])
		}
		pane := "System Settings → Privacy & Security"
		if compareMac(in.plat.macos, "12") <= 0 {
			pane = "System Preferences → Security & Privacy → Privacy"
		}
		text = "{cask} is not signed and requires " + access + " access,\nso you will need to re-grant " + access +
			" access every time the app is updated.\n\nEnable or re-enable it in:\n  " + pane + " → " + access +
			"\nTo re-enable, untick and retick {cask}.app.\n"
		ok = true
		args = nil
	case "puts":
		for _, a := range args {
			b.custom = append(b.custom, str(a))
		}
		return sym("__builtin"), true
	}
	if !ok {
		return nil, false
	}
	text = strings.ReplaceAll(text, "{cask}", b.token)
	if len(args) > 0 {
		text = strings.ReplaceAll(text, "{0}", str(args[0]))
	}
	var key strings.Builder
	key.WriteString(c.name)
	for _, a := range args {
		key.WriteString("\x00" + str(a))
	}
	for i, bc := range b.builtin {
		if bc.name == key.String() {
			b.builtin[i].text = text
			return sym("__builtin"), true
		}
	}
	b.builtin = append(b.builtin, builtinCaveat{key.String(), text})
	return sym("__builtin"), true
}

func (b *caskB) caveats() value {
	parts := append([]string{}, b.custom...)
	for _, bc := range b.builtin {
		parts = append(parts, bc.text)
	}
	s := strings.Join(parts, "\n")
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return rstr(s)
}

func (b *caskB) hash(brew bool) *hash {
	h := &hash{}
	h.put("token", rstr(b.token))
	h.put("name", strs(b.names))
	h.put("desc", b.desc)
	h.put("homepage", b.homepage)
	h.put("url", b.url)
	if b.urlSpecs != nil {
		h.put("url_specs", b.urlSpecs)
	} else {
		h.put("url_specs", nil)
	}
	h.put("version", b.version)
	h.put("sha256", b.sha256)
	h.put("artifacts", b.artifactsList(brew))
	h.put("caveats", b.caveats())
	if b.rosetta {
		h.put("caveats_rosetta", rbool(true))
	} else {
		h.put("caveats_rosetta", nil)
	}
	if b.depends != nil {
		h.put("depends_on", b.depends)
	} else {
		h.put("depends_on", &hash{})
	}
	if b.conflicts != nil {
		h.put("conflicts_with", b.conflicts)
	} else {
		h.put("conflicts_with", nil)
	}
	if b.container != nil {
		h.put("container", b.container)
	} else {
		h.put("container", nil)
	}
	h.put("rename", &arr{b.rename})
	h.put("auto_updates", b.autoUpd)
	h.put("deprecated", rbool(b.deprecated))
	h.put("deprecation_date", b.deprecationDate)
	h.put("deprecation_reason", b.deprecationReason)
	h.put("deprecation_replacement_formula", b.depReplF)
	h.put("deprecation_replacement_cask", b.depReplC)
	h.put("disabled", rbool(b.disabled))
	h.put("disable_date", b.disableDate)
	h.put("disable_reason", b.disableReason)
	h.put("disable_replacement_formula", b.disReplF)
	h.put("disable_replacement_cask", b.disReplC)
	var langs []string
	for _, lb := range b.langs {
		langs = append(langs, lb.codes...)
	}
	h.put("languages", strs(langs))
	return h
}

func (b *caskB) artifactsList(brew bool) *arr {
	arts := append([]artifact{}, b.artifacts...)
	for i, a := range arts {
		if strings.HasSuffix(a.key, "flight") && !brew {
			steps := &hash{}
			steps.put("steps", &arr{a.flight})
			arts[i] = artifact{key: a.key + "_steps", args: []value{steps}}
		}
	}
	sort.SliceStable(arts, func(i, j int) bool { return artifactOrder[arts[i].key] < artifactOrder[arts[j].key] })
	out := &arr{}
	for _, a := range arts {
		h := &hash{}
		if strings.HasSuffix(a.key, "flight") {
			h.put(a.key, nil)
			out.v = append(out.v, h)
			continue
		}
		h.put(a.key, &arr{a.args})
		if a.hasTgt {
			h.put("target", rstr(a.target))
		}
		out.v = append(out.v, h)
	}
	return out
}

func compactBlank(vs []value) []value {
	var out []value
	for _, v := range vs {
		if !blank(v) {
			out = append(out, v)
		}
	}
	return out
}

func blank(v value) bool {
	switch x := v.(type) {
	case nil:
		return true
	case rbool:
		return !bool(x)
	case rstr:
		return strings.TrimSpace(string(x)) == ""
	case ver:
		return strings.TrimSpace(string(x)) == ""
	case *arr:
		return len(x.v) == 0
	case *hash:
		return len(x.keys) == 0
	}
	return false
}
