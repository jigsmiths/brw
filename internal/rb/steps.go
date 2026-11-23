package rb

import (
	"strconv"
	"strings"
)

// stepsDSL mirrors Homebrew::InstallSteps::DSL for casks: every base defaults to staged_path.
type stepsDSL struct {
	steps   []value
	guards  []value
	guardID int
}

var absoluteTokens = strings.Fields(`HOMEBREW_PREFIX HOMEBREW_CELLAR prefix opt_prefix bin sbin lib libexec share pkgshare var etc pkgetc
staged_path appdir caskroom_path temp rack bash_completion zsh_completion fish_completion pwsh_completion`)

func defaultBase(p string) value {
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "~") {
		return nil
	}
	for _, t := range absoluteTokens {
		if strings.HasPrefix(p, "{{"+t+"}}") {
			return nil
		}
	}
	return rstr("staged_path")
}

func pathSpec(p value, base value, formula value, useDefault bool) *hash {
	h := &hash{}
	b := base
	if b == nil && useDefault {
		b = defaultBase(str(p))
	}
	if b != nil {
		h.put("base", rstr(str(b)))
	}
	if formula != nil {
		h.put("formula", formula)
	}
	h.put("path", rstr(str(p)))
	return compactHash(h)
}

func pathSpecs(ps value, base value, useDefault bool) *arr {
	a, ok := ps.(*arr)
	if !ok {
		a = &arr{[]value{ps}}
	}
	out := &arr{}
	for _, p := range a.v {
		out.v = append(out.v, pathSpec(p, base, nil, useDefault))
	}
	return out
}

func compactHash(h *hash) *hash {
	out := &hash{}
	for i, k := range h.keys {
		if !blank(h.vals[i]) {
			out.set(k, h.vals[i])
		}
	}
	return out
}

func deepCompact(v value) value {
	switch x := v.(type) {
	case *hash:
		out := &hash{}
		for i, k := range x.keys {
			c := deepCompact(x.vals[i])
			if !blank(c) {
				out.set(k, c)
			}
		}
		return out
	case *arr:
		out := &arr{}
		for _, e := range x.v {
			c := deepCompact(e)
			if !blank(c) {
				out.v = append(out.v, c)
			}
		}
		return out
	case sym:
		return rstr(x)
	}
	return v
}

var preserved = []string{"after", "args", "before", "content", "env", "overwrite"}

func (d *stepsDSL) add(typ string, fields *hash) {
	if len(d.guards) > 0 {
		fields.put("guards", &arr{append([]value{}, d.guards...)})
	}
	fields.put("type", rstr(typ))
	step := &hash{}
	keep := map[string]value{}
	for i, k := range fields.keys {
		ks := str(k)
		isPres := false
		for _, p := range preserved {
			if p == ks {
				isPres = true
			}
		}
		if isPres {
			keep[ks] = fields.vals[i]
			continue
		}
		c := deepCompact(fields.vals[i])
		if !blank(c) {
			step.put(ks, c)
		}
	}
	for _, p := range preserved {
		v, ok := keep[p]
		if !ok || v == nil {
			continue
		}
		if (p == "args" || p == "env") && blank(v) {
			continue
		}
		if p == "args" {
			a := &arr{}
			for _, e := range v.(*arr).v {
				a.v = append(a.v, rstr(str(e)))
			}
			v = a
		}
		step.put(p, v)
	}
	d.steps = append(d.steps, step)
}

func kwOr(kw *hash, k string, def value) value {
	if kw == nil {
		return def
	}
	if v, ok := kw.get(sym(k)); ok {
		return v
	}
	return def
}

func symStr(v value) value {
	if s, ok := v.(sym); ok {
		return rstr(s)
	}
	return v
}

func (d *stepsDSL) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	a := func(i int) value {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	no, yes := rbool(false), rbool(true)
	f := &hash{}
	switch c.name {
	case "version":
		return tmplV{}, true
	case "token":
		return rstr("{{token}}"), true
	case "arch":
		return rstr("{{arch}}"), true
	case "if_path_exists", "unless_path_exists", "on_macos", "on_linux":
		var g *hash
		switch c.name {
		case "on_macos", "on_linux":
			g = &hash{}
			g.put("condition", rstr("on"))
			g.put("value", rstr(strings.TrimPrefix(c.name, "on_")))
		default:
			g = pathSpec(a(0), kwGet(kw, "base"), nil, true)
			g.put("condition", rstr(strings.TrimSuffix(c.name, "_path_exists")+"_exists"))
		}
		d.guardID++
		g.put("id", rstr(strconv.Itoa(d.guardID)))
		prev := d.guards
		d.guards = append(append([]value{}, d.guards...), g)
		in.run(c.block.body, &scope{vars: map[string]value{}, parent: s})
		d.guards = prev
		return nil, true
	case "mkdir_p", "touch":
		f.put("path", pathSpec(a(0), kwGet(kw, "base"), nil, true))
		d.add(c.name, f)
	case "move", "copy", "symlink", "move_contents", "symlink_tree", "symlink_children", "install_gzipped_executable":
		f.put("source", pathSpec(a(0), kwGet(kw, "source_base"), kwGet(kw, "source_formula"), true))
		tgt := a(1)
		if tgt == nil && c.name == "symlink_children" {
			tgt = a(0)
		}
		tb := kwGet(kw, "target_base")
		if tb == nil && (c.name == "symlink_tree" || c.name == "symlink_children") {
			tb = rstr("homebrew_prefix")
		}
		f.put("target", pathSpec(tgt, tb, kwGet(kw, "target_formula"), true))
		typ := c.name
		switch c.name {
		case "move":
			f.put("force", kwOr(kw, "force", no))
			f.put("overwrite", kwOr(kw, "overwrite", yes))
			f.put("source_glob", kwOr(kw, "source_glob", no))
		case "copy":
			f.put("recursive", kwOr(kw, "recursive", no))
			if !truthy(kwOr(kw, "overwrite", yes)) {
				f.put("overwrite", no)
			} else {
				f.put("overwrite", nil)
			}
			f.put("source_glob", kwOr(kw, "source_glob", no))
		case "symlink":
			if truthy(kwOr(kw, "force", no)) || truthy(kwOr(kw, "overwrite", no)) {
				f.put("force", yes)
			}
			if truthy(kwOr(kw, "uninstall", no)) || truthy(kwOr(kw, "remove_on_uninstall", no)) {
				f.put("uninstall", yes)
			}
			f.put("source_glob", kwOr(kw, "source_glob", no))
			f.put("sudo", symStr(kwOr(kw, "sudo", no)))
		case "symlink_tree":
			typ = "link_dir"
		case "symlink_children":
			typ = "link_children"
			f.put("prefix", kwOr(kw, "prefix", rstr("")))
			f.put("suffix", kwOr(kw, "suffix", rstr("")))
		}
		d.add(typ, f)
	case "remove":
		f.put("paths", pathSpecs(a(0), kwGet(kw, "base"), true))
		f.put("recursive", kwOr(kw, "recursive", no))
		f.put("sudo", symStr(kwOr(kw, "sudo", no)))
		f.put("symlink_target_contains", kwGet(kw, "symlink_target_contains"))
		f.put("content_contains", kwGet(kw, "content_contains"))
		d.add("remove", f)
	case "set_permissions":
		f.put("paths", pathSpecs(a(0), kwGet(kw, "base"), true))
		f.put("permissions", a(1))
		f.put("non_recursive", rbool(!truthy(kwOr(kw, "recursive", yes))))
		d.add("set_permissions", f)
	case "set_ownership":
		f.put("paths", pathSpecs(a(0), kwGet(kw, "base"), true))
		f.put("user", kwGet(kw, "user"))
		if g := kwOr(kw, "group", rstr("staff")); str(g) != "staff" {
			f.put("group", g)
		}
		f.put("non_recursive", rbool(!truthy(kwOr(kw, "recursive", yes))))
		d.add("set_ownership", f)
	case "run":
		f.put("command", pathSpec(a(0), kwGet(kw, "base"), nil, false))
		f.put("args", kwOr(kw, "args", &arr{}))
		f.put("env", kwOr(kw, "env", &hash{}))
		f.put("sudo", kwOr(kw, "sudo", no))
		f.put("allow_failure", rbool(!truthy(kwOr(kw, "must_succeed", yes))))
		f.put("print_stdout", kwOr(kw, "print_stdout", no))
		f.put("suppress_stderr", rbool(!truthy(kwOr(kw, "print_stderr", yes))))
		for _, k := range []string{"stdin_path", "stdout_path", "chdir"} {
			if v := kwGet(kw, k); v != nil {
				f.put(k, pathSpec(v, nil, nil, true))
			}
		}
		f.put("writable_paths", pathSpecs(kwOr(kw, "writable_paths", &arr{}), kwGet(kw, "writable_base"), true))
		f.put("network_access", kwOr(kw, "network_access", no))
		d.add("run", f)
	case "terminate_process":
		f.put("name", a(0))
		if m := str(kwOr(kw, "match", sym("name"))); m != "name" {
			f.put("match", rstr(m))
		}
		f.put("sudo", kwOr(kw, "sudo", no))
		if at := kwOr(kw, "attempts", rint(1)); at != rint(1) {
			f.put("attempts", at)
		}
		f.put("must_succeed", kwOr(kw, "must_succeed", no))
		f.put("notices", kwOr(kw, "notices", &arr{}))
		f.put("failure_message", kwGet(kw, "failure_message"))
		d.add("terminate_process", f)
	case "delete_keychain_certificates", "delete_keychain_certificate":
		f.put("name", a(0))
		m := kwGet(kw, "fingerprint_of")
		if c.name == "delete_keychain_certificate" {
			m = kwGet(kw, "matching_certificate")
		}
		if m != nil {
			f.put("matching_certificate", pathSpec(m, kwGet(kw, "base"), nil, false))
		}
		d.add("delete_keychain_certificate", f)
	case "write_file":
		content := str(a(1))
		if truthy(kwOr(kw, "append_newline", no)) && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		f.put("path", pathSpec(a(0), kwGet(kw, "base"), nil, true))
		f.put("content", rstr(content))
		if truthy(kwOr(kw, "overwrite", yes)) {
			f.put("overwrite", yes)
		}
		d.add("write", f)
	case "warn":
		f.put("message", a(0))
		d.add("warn", f)
	case "inreplace":
		f.put("path", pathSpec(a(0), kwGet(kw, "base"), nil, true))
		before := a(1)
		if r, ok := before.(*regex); ok {
			f.put("before", rstr(r.src))
			f.put("regexp", yes) // regexp_options is left out when 0, as brew's is
		} else {
			f.put("before", before)
		}
		f.put("after", a(2))
		f.put("skip_audit", rbool(!truthy(kwOr(kw, "audit_result", yes))))
		f.put("first_only", rbool(!truthy(kwOr(kw, "global", yes))))
		d.add("inreplace", f)
	default:
		return nil, false
	}
	return nil, true
}
