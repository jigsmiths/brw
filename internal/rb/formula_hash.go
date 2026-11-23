package rb

import (
	"slices"
	"strconv"
	"strings"
)

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func licenseString(v value, bracket bool) value {
	switch x := v.(type) {
	case nil:
		return nil
	case rstr:
		return x
	case sym:
		return rstr("LicenseRef-Homebrew-" + strings.ReplaceAll(string(x), "_", "-"))
	case *hash:
		var parts []string
		op := " AND "
		if len(x.keys) == 1 {
			k, val := x.keys[0], x.vals[0]
			if ks, ok := k.(rstr); ok {
				with, _ := val.(*hash)
				parts = append(parts, string(ks)+" WITH "+str(kwGet(with, "with")))
			} else {
				if k == sym("any_of") {
					op = " OR "
				}
				a, ok := val.(*arr)
				if !ok {
					fail("license %v", k)
				}
				for _, e := range a.v {
					parts = append(parts, str(licenseString(e, true)))
				}
			}
		} else {
			bracket = false
			for i, k := range x.keys {
				parts = append(parts, str(licenseString(&hash{keys: []value{k}, vals: []value{x.vals[i]}}, true)))
			}
		}
		s := strings.Join(parts, op)
		if bracket {
			s = "(" + s + ")"
		}
		return rstr(s)
	}
	fail("license %T", v)
	return nil
}

func uniq(ss []string) []string {
	out := []string{}
	for _, s := range ss {
		if !slices.Contains(out, s) {
			out = append(out, s)
		}
	}
	return out
}

func strsOrEmpty(ss []string) *arr {
	a := &arr{v: []value{}}
	for _, s := range ss {
		a.v = append(a.v, rstr(s))
	}
	return a
}

func depsHash(deps []dep) *hash {
	pick := func(keep func(dep) bool) *arr {
		var names []string
		for _, d := range deps {
			if !d.macos && keep(d) {
				names = append(names, d.name)
			}
		}
		return strsOrEmpty(uniq(names))
	}
	h := &hash{}
	h.put("build_dependencies", pick(func(d dep) bool { return d.has("build") }))
	h.put("dependencies", pick(func(d dep) bool {
		return !d.has("optional") && !d.has("recommended") && !d.has("build") && !d.has("test")
	}))
	h.put("test_dependencies", pick(func(d dep) bool { return d.has("test") }))
	h.put("recommended_dependencies", pick(func(d dep) bool { return d.has("recommended") }))
	h.put("optional_dependencies", pick(func(d dep) bool { return d.has("optional") }))
	h.put("no_linkage_dependencies", pick(func(d dep) bool { return d.has("no_linkage") }))
	uses, bounds := &arr{v: []value{}}, &arr{v: []value{}}
	var seen []string
	for _, d := range deps {
		if !d.macos {
			continue
		}
		key := d.name + "\x00" + strings.Join(d.tags, ",")
		if slices.Contains(seen, key) {
			continue
		}
		seen = append(seen, key)
		switch len(d.tags) {
		case 0:
			uses.v = append(uses.v, rstr(d.name))
		case 1:
			h := &hash{}
			h.put(d.name, rstr(d.tags[0]))
			uses.v = append(uses.v, h)
		default:
			h := &hash{}
			h.put(d.name, strsOrEmpty(d.tags))
			uses.v = append(uses.v, h)
		}
		bh := &hash{}
		for i, k := range d.bounds.keys {
			bh.set(rstr(str(k)), rstr(str(d.bounds.vals[i])))
		}
		bounds.v = append(bounds.v, bh)
	}
	h.put("uses_from_macos", uses)
	h.put("uses_from_macos_bounds", bounds)
	return h
}

func sameDeps(a, b []dep) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].name != b[i].name || a[i].macos != b[i].macos || !slices.Equal(a[i].tags, b[i].tags) {
			return false
		}
	}
	return true
}

func (b *formulaB) requirements() *arr {
	out := &arr{v: []value{}}
	type entry struct {
		r     req
		specs []string
	}
	var all []*entry
	add := func(reqs []req, spec string) {
		for _, r := range reqs {
			var found *entry
			for _, e := range all {
				if e.r.name == r.name && e.r.version == r.version && slices.Equal(e.r.contexts, r.contexts) {
					found = e
				}
			}
			if found == nil {
				found = &entry{r: r}
				all = append(all, found)
			}
			if !slices.Contains(found.specs, spec) {
				found.specs = append(found.specs, spec)
			}
		}
	}
	if b.stable != nil {
		add(b.stable.reqs, "stable")
	}
	if b.hasHead {
		add(b.head.reqs, "head")
	}
	for _, e := range all {
		h := &hash{}
		h.put("name", rstr(e.r.name))
		h.put("cask", nil)
		h.put("download", nil)
		if e.r.version != "" {
			h.put("version", rstr(e.r.version))
		} else {
			h.put("version", nil)
		}
		h.put("contexts", strsOrEmpty(e.r.contexts))
		h.put("specs", strsOrEmpty(e.specs))
		out.v = append(out.v, h)
	}
	return out
}

func orNil(v value) value {
	if s, ok := v.(rstr); ok && s == "" {
		return nil
	}
	return v
}

func symName(v value) value {
	if s, ok := v.(sym); ok {
		return rstr(s)
	}
	return v
}

func lifecycleArgs(date, because, rf, rc value) value {
	if date == nil {
		return nil
	}
	h := &hash{}
	h.put("date", date)
	h.put("because", symName(because))
	h.put("replacement_formula", rf)
	h.put("replacement_cask", rc)
	return h
}

func (b *formulaB) caveats() value {
	d, ok := b.defs["caveats"]
	if !ok {
		return nil
	}
	in := &interp{plat: b.plat, ctx: []dsl{b.instance()}}
	var v value
	func() {
		defer func() {
			if r := recover(); r != nil {
				if _, ok := r.(returned); !ok {
					panic(r)
				}
			}
		}()
		v = in.callDef(d, nil, nil, &scope{vars: map[string]value{}})
	}()
	if v == nil || str(v) == "" {
		return nil
	}
	return rstr(str(v))
}

func (b *formulaB) hash() *hash {
	h := &hash{}
	h.put("name", rstr(b.name))
	h.put("desc", b.desc)
	h.put("license", licenseString(b.license, false))
	h.put("homepage", b.home)
	versions := &hash{}
	if b.stable != nil {
		versions.put("stable", rstr(b.version()))
	} else {
		versions.put("stable", nil)
	}
	if b.hasHead {
		versions.put("head", rstr("HEAD"))
	} else {
		versions.put("head", nil)
	}
	versions.put("bottle", rbool(b.bottle != nil && b.stable != nil))
	h.put("versions", versions)

	urls := &hash{}
	if b.stable != nil {
		u := &hash{}
		u.put("url", rstr(b.stable.url))
		u.put("tag", kwGet(b.stable.specs, "tag"))
		u.put("revision", kwGet(b.stable.specs, "revision"))
		u.put("using", usingSym(b.stable.using))
		u.put("checksum", orNil(rstr(b.stable.checksum)))
		urls.put("stable", u)
	}
	if b.hasHead {
		u := &hash{}
		u.put("url", rstr(b.head.url))
		u.put("branch", kwGet(b.head.specs, "branch"))
		u.put("using", usingSym(b.head.using))
		urls.put("head", u)
	}
	h.put("urls", urls)

	patches := &arr{v: []value{}}
	if b.stable != nil {
		patches.v = append(patches.v, b.stable.patches...)
	}
	h.put("patches", patches)
	h.put("local_patch_checksums", &hash{})
	h.put("revision", rint(b.revision))
	h.put("version_scheme", rint(b.scheme))
	h.put("compatibility_version", b.compat)
	h.put("autobump", rbool(b.noAutobump == nil))
	h.put("no_autobump_message", b.noAutobump)
	h.put("skip_livecheck", rbool(b.skipLivecheck))

	bottle := &hash{}
	if b.bottle != nil && b.stable != nil {
		bottle.put("stable", b.bottle.hash(b.name, b.pkgVersion()))
	}
	h.put("bottle", bottle)
	h.put("pour_bottle_only_if", b.pourOnlyIf)
	h.put("keg_only", rbool(b.kegOnly != nil))
	if b.kegOnly != nil {
		h.put("keg_only_reason", b.kegOnly)
	} else {
		h.put("keg_only_reason", nil)
	}
	options := &arr{v: []value{}}
	for _, o := range b.options {
		options.v = append(options.v, o)
	}
	h.put("options", options)

	var stableDeps []dep
	if b.stable != nil {
		stableDeps = b.stable.deps
	}
	dh := depsHash(stableDeps)
	if b.stable == nil {
		dh = depsHash(nil)
	}
	for i, k := range dh.keys {
		h.set(k, dh.vals[i])
	}
	if b.hasHead && (b.stable == nil || !sameDeps(b.head.deps, b.stable.deps)) {
		h.put("head_dependencies", depsHash(b.head.deps))
	}
	h.put("requirements", b.requirements())
	h.put("conflicts_with", strsOrEmpty(b.conflicts))
	why := &arr{v: []value{}}
	for _, w := range b.conflictWhy {
		if w == nil {
			why.v = append(why.v, nil)
		} else {
			why.v = append(why.v, rstr(str(w)))
		}
	}
	h.put("conflicts_with_reasons", why)
	h.put("link_overwrite", strsOrEmpty(b.linkOverwrite))
	h.put("caveats", b.caveats())
	h.put("installed", &arr{v: []value{}})
	h.put("linked_keg", nil)
	h.put("pinned", rbool(false))
	h.put("outdated", rbool(false))
	h.put("deprecated", rbool(b.deprecated))
	h.put("deprecation_date", b.deprecationDate)
	h.put("deprecation_reason", symName(b.deprecationReason))
	h.put("deprecation_replacement_formula", b.depReplF)
	h.put("deprecation_replacement_cask", b.depReplC)
	h.put("deprecate_args", b.deprecateArgs)
	h.put("disabled", rbool(b.disabled))
	h.put("disable_date", b.disableDate)
	h.put("disable_reason", symName(b.disableReason))
	h.put("disable_replacement_formula", b.disReplF)
	h.put("disable_replacement_cask", b.disReplC)
	h.put("disable_args", b.disableArgs)
	steps := &arr{v: []value{}}
	if b.postSteps != nil {
		steps.v = append(steps.v, b.postSteps.steps...)
	}
	h.put("post_install_steps", steps)
	_, defined := b.defs["post_install"]
	h.put("post_install_defined", rbool(defined))
	if b.service != nil {
		h.put("service", b.service.hash())
	} else {
		h.put("service", nil)
	}
	return h
}

func usingSym(v value) value {
	if s, ok := v.(sym); ok {
		return rstr(s)
	}
	return nil
}
