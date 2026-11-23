package rb

import (
	"net/url"
	"path"
	"regexp"
	"strings"
)

// activeMacOS are the macOS releases brew still names in bottle tags.
var activeMacOS = map[string]bool{
	"golden_gate": true, "tahoe": true, "sequoia": true, "sonoma": true, "ventura": true, "monterey": true, "big_sur": true,
}

// bottleTag is Utils::Bottles::Tag's name for a tag symbol: x86_64 macOS tags
// of supported releases drop the arch.
func bottleTag(s string) string {
	if s == "all" {
		return s
	}
	arch, system := "x86_64", s
	for _, a := range []string{"x86_64", "arm64", "aarch64", "arm", "intel", "ppc64le", "ppc64", "ppc", "i386"} {
		if rest, ok := strings.CutPrefix(s, a+"_"); ok {
			arch, system = a, rest
			break
		}
	}
	switch arch {
	case "intel":
		arch = "x86_64"
	case "arm", "aarch64":
		arch = "arm64"
	}
	if activeMacOS[system] && arch == "x86_64" {
		return system
	}
	return arch + "_" + system
}

func defaultCellar(tag string) string {
	switch {
	case strings.HasSuffix(tag, "_linux"):
		return "/home/linuxbrew/.linuxbrew/Cellar"
	case strings.HasPrefix(tag, "arm64_"):
		return "/opt/homebrew/Cellar"
	}
	return "/usr/local/Cellar"
}

type bottleFile struct {
	tag, sha256 string
	cellar      value
}

// bottleB is a bottle block.
type bottleB struct {
	rootURL  string
	rebuild  int64
	manifest string
	files    []bottleFile
}

const defaultBottleDomain = "https://ghcr.io/v2/homebrew/core"

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

func (b *bottleB) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	switch c.name {
	case "root_url":
		if len(args) == 0 {
			fail("root_url")
		}
		b.rootURL = str(args[0])
	case "rebuild":
		b.rebuild = int64(args[0].(rint))
	case "manifest_sha256":
		if len(args) != 1 || !sha256Hex.MatchString(str(args[0])) {
			fail("manifest_sha256")
		}
		b.manifest = strings.ToLower(str(args[0]))
	case "sha256":
		if kw == nil {
			fail("bottle sha256 %v", args)
		}
		f := bottleFile{}
		for i, k := range kw.keys {
			switch k {
			case sym("cellar"):
				f.cellar = kw.vals[i]
			default:
				ks, ok := k.(sym)
				v, isStr := kw.vals[i].(rstr)
				if !ok || !isStr || !sha256Hex.MatchString(string(v)) {
					fail("bottle sha256 %v", k)
				}
				f.tag, f.sha256 = bottleTag(string(ks)), strings.ToLower(string(v))
			}
		}
		if f.tag == "" {
			fail("bottle sha256 without a tag")
		}
		if f.cellar == nil {
			f.cellar = rstr(defaultCellar(f.tag))
		}
		b.files = append(b.files, f)
	default:
		return nil, false
	}
	return nil, true
}

func (b *bottleB) hash(name, pkgVersion string) *hash {
	root := b.rootURL
	if root == "" {
		root = defaultBottleDomain
	}
	h := &hash{}
	h.put("rebuild", rint(b.rebuild))
	h.put("root_url", rstr(root))
	if b.manifest != "" {
		h.put("manifest_sha256", rstr(b.manifest))
	}
	files := &hash{}
	for _, f := range b.files {
		ext := "." + f.tag + ".bottle"
		if b.rebuild > 0 {
			ext += "." + itoa(b.rebuild)
		}
		ext += ".tar.gz"
		var p string
		if strings.HasPrefix(root, "https://ghcr.io/v2/") || strings.HasPrefix(root, "docker://ghcr.io/") {
			image := strings.NewReplacer("@", "/", "+", "x").Replace(name)
			p = image + "/blobs/sha256:" + f.sha256
		} else {
			p = url.PathEscape(name + "-" + pkgVersion + ext)
		}
		fh := &hash{}
		cellar := f.cellar
		if s, ok := cellar.(sym); ok {
			cellar = rstr(":" + string(s))
		}
		fh.put("cellar", cellar)
		fh.put("url", rstr(root+"/"+p))
		fh.put("sha256", rstr(f.sha256))
		files.put(f.tag, fh)
	}
	h.put("files", files)
	return h
}

// instanceDSL answers what a Formula instance knows: its paths, name and
// version. Paths use the API's placeholders.
type instanceDSL struct{ f *formulaB }

func (b *formulaB) instance() *instanceDSL { return &instanceDSL{b} }

func (d *instanceDSL) prefix() string {
	return PrefixPlaceholder + "/Cellar/" + d.f.name + "/" + d.f.pkgVersion()
}

// formulaPath gives a Formula's path method relative to prefix, or false.
func formulaPath(prefix, optPrefix, name, method string) (string, bool) {
	sub := map[string]string{
		"bin": "bin", "sbin": "sbin", "lib": "lib", "libexec": "libexec", "share": "share", "include": "include",
		"frameworks": "Frameworks", "kext_prefix": "Library/Extensions", "man": "share/man", "info": "share/info",
		"doc": "share/doc/" + name, "pkgshare": "share/" + name, "elisp": "share/emacs/site-lisp/" + name,
		"bash_completion": "etc/bash_completion.d", "zsh_completion": "share/zsh/site-functions",
		"fish_completion": "share/fish/vendor_completions.d", "pwsh_completion": "share/pwsh/completions",
		"zsh_function": "share/zsh/site-functions",
	}
	for i := 1; i <= 8; i++ {
		sub["man"+itoa(int64(i))] = "share/man/man" + itoa(int64(i))
	}
	switch method {
	case "prefix":
		return prefix, true
	case "opt_prefix":
		return optPrefix, true
	case "etc":
		return PrefixPlaceholder + "/etc", true
	case "pkgetc":
		return PrefixPlaceholder + "/etc/" + name, true
	case "var":
		return PrefixPlaceholder + "/var", true
	case "rack":
		return PrefixPlaceholder + "/Cellar/" + name, true
	}
	if rel, ok := sub[method]; ok {
		return path.Join(prefix, rel), true
	}
	if m, ok := strings.CutPrefix(method, "opt_"); ok {
		if rel, ok := sub[m]; ok {
			return path.Join(optPrefix, rel), true
		}
	}
	return "", false
}

func (d *instanceDSL) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	f := d.f
	if def, ok := f.defs[c.name]; ok {
		return in.callDef(def, args, kw, s), true
	}
	if ok, isOn := onCondition(c.name, args, in.plat); isOn {
		if c.block == nil {
			fail("%s without block", c.name)
		}
		if !ok {
			return nil, true
		}
		return in.run(c.block.body, &scope{vars: map[string]value{}, parent: s}), true
	}
	if p, ok := formulaPath(d.prefix(), PrefixPlaceholder+"/opt/"+f.name, f.name, c.name); ok && len(args) == 0 {
		return rstr(p), true
	}
	switch c.name {
	case "name":
		return rstr(f.name), true
	case "version":
		return ver(f.version()), true
	case "pkg_version":
		return rstr(f.pkgVersion()), true
	case "revision":
		return rint(f.revision), true
	case "full_name":
		return rstr(f.name), true
	case "plist_name", "service_name":
		return rstr("sh.brew." + f.name), true
	case "std_service_path_env":
		return rstr(PrefixPlaceholder + "/bin:" + PrefixPlaceholder + "/sbin:/usr/bin:/bin:/usr/sbin:/sbin"), true
	}
	return nil, false
}

// passwd is Etc.getpwuid: the user running brw.
type passwd string

func (passwd) rubyValue() {}

// formulaRef is Formula["name"] in a formula: another formula, of which
// only the opt paths are known before it is installed.
type formulaRef struct{ name string }

func (formulaRef) rubyValue() {}

func (r formulaRef) method(name string) value {
	if !strings.HasPrefix(name, "opt_") {
		fail("Formula[%q].%s", r.name, name)
	}
	p, ok := formulaPath("", PrefixPlaceholder+"/opt/"+r.name, r.name, name)
	if !ok {
		fail("Formula[%q].%s", r.name, name)
	}
	return rstr(p)
}

// serviceB is a service block, as Homebrew::Service#to_hash writes it.
type serviceB struct {
	f      *formulaB
	fields *hash
	cron   string
}

var serviceSetters = map[string]bool{
	"working_dir": true, "root_dir": true, "input_path": true, "log_path": true, "error_log_path": true,
	"interval": true, "launch_only_once": true, "require_root": true, "restart_delay": true,
	"throttle_interval": true, "stop_timeout": true, "nice": true, "process_type": true, "run_type": true,
	"macos_legacy_timers": true, "run_at_load": true,
}

func (sv *serviceB) put(k string, v value) {
	if sv.fields == nil {
		sv.fields = &hash{}
	}
	sv.fields.put(k, v)
}

func (sv *serviceB) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	arg := func() value {
		if len(args) == 0 {
			fail("service %s without a value", c.name)
		}
		return args[0]
	}
	switch {
	case c.name == "run":
		switch {
		case len(args) == 1:
			sv.put("run", runParam(args[0]))
		case kw != nil:
			h := &hash{}
			for _, os := range []string{"macos", "linux"} {
				if v := kwGet(kw, os); v != nil {
					h.put(os, runParam(v))
				}
			}
			sv.put("run", h)
		default:
			fail("service run")
		}
	case c.name == "name":
		h := &hash{}
		for _, os := range []string{"macos", "linux"} {
			if v := kwGet(kw, os); v != nil {
				h.put(os, rstr(str(v)))
			}
		}
		sv.put("name", h)
	case c.name == "keep_alive":
		if len(args) == 0 && kw != nil {
			sv.put("keep_alive", kw)
			break
		}
		switch v := arg().(type) {
		case rbool:
			h := &hash{}
			h.put("always", v)
			sv.put("keep_alive", h)
		case *hash:
			sv.put("keep_alive", v)
		default:
			fail("keep_alive %v", v)
		}
	case c.name == "environment_variables":
		if kw == nil {
			fail("environment_variables")
		}
		h := &hash{}
		for i, k := range kw.keys {
			h.set(k, rstr(str(kw.vals[i])))
		}
		sv.put("environment_variables", h)
	case c.name == "sockets":
		sv.put("sockets", arg())
	case c.name == "cron":
		sv.cron = str(arg())
	case serviceSetters[c.name]:
		v := arg()
		switch v.(type) {
		case rstr, ver:
			v = rstr(str(v))
		}
		sv.put(c.name, v)
	default:
		return nil, false
	}
	return nil, true
}

func runParam(v value) value {
	switch x := v.(type) {
	case *arr:
		out := &arr{}
		for _, e := range x.v {
			out.v = append(out.v, rstr(str(e)))
		}
		return out
	case rstr, ver:
		return rstr(str(x))
	}
	fail("service run %v", v)
	return nil
}

func (sv *serviceB) hash() *hash {
	h := &hash{}
	if sv.fields == nil {
		return h
	}
	run, _ := sv.fields.get(rstr("run"))
	name, _ := sv.fields.get(rstr("name"))
	if run == nil {
		if name != nil {
			h.put("name", name)
		}
		return h
	}
	get := func(k string) value {
		v, _ := sv.fields.get(rstr(k))
		return v
	}
	runType := get("run_type")
	if runType == nil {
		runType = sym("immediate")
	}
	for _, k := range []string{"name", "run", "run_type", "interval", "cron", "keep_alive", "launch_only_once",
		"require_root", "environment_variables", "working_dir", "root_dir", "input_path", "log_path",
		"error_log_path", "restart_delay", "throttle_interval", "stop_timeout", "nice", "process_type",
		"macos_legacy_timers", "sockets"} {
		var v value
		switch k {
		case "run_type":
			v = runType
		case "cron":
			if sv.cron != "" {
				v = rstr(parseCron(sv.cron))
			}
		default:
			v = get(k)
		}
		if !blank(v) {
			h.put(k, v)
		}
	}
	return h
}

// parseCron is Service#parse_cron joined as Service#to_hash writes it.
func parseCron(s string) string {
	f := map[string]string{"Minute": "*", "Hour": "*", "Day": "*", "Month": "*", "Weekday": "*"}
	switch s {
	case "@hourly":
		f["Minute"] = "0"
	case "@daily":
		f["Minute"], f["Hour"] = "0", "0"
	case "@weekly":
		f["Minute"], f["Hour"], f["Weekday"] = "0", "0", "0"
	case "@monthly":
		f["Minute"], f["Hour"], f["Day"] = "0", "0", "1"
	default:
		parts := strings.Fields(s)
		if len(parts) != 5 {
			fail("cron %q", s)
		}
		for i, k := range []string{"Minute", "Hour", "Day", "Month", "Weekday"} {
			f[k] = parts[i]
		}
	}
	return f["Minute"] + " " + f["Hour"] + " " + f["Day"] + " " + f["Month"] + " " + f["Weekday"]
}
