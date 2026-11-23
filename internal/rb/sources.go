package rb

import (
	"fmt"
	"path/filepath"
	"strconv"
	"sync/atomic"
)

type Resource struct {
	Name, URL, SHA256 string
}

// Data is the text after the formula's __END__.
type Patch struct {
	Strip       string
	URL, SHA256 string
	Data        string
}

func (f *Formula) Resources() []Resource {
	sp := f.b.active()
	if sp == nil {
		return nil
	}
	var out []Resource
	for _, r := range sp.resource {
		out = append(out, Resource{Name: r.name, URL: r.url, SHA256: r.checksum})
	}
	return out
}

// Patches are in the order brew applies them.
func (f *Formula) Patches() []Patch {
	sp := f.b.active()
	if sp == nil {
		return nil
	}
	var out []Patch
	for _, h := range sp.patches {
		p, ok := h.(*hash)
		if !ok {
			continue
		}
		get := func(k string) string {
			v, _ := p.get(rstr(k))
			if v == nil {
				return ""
			}
			return str(v)
		}
		patch := Patch{Strip: get("strip"), URL: get("url"), SHA256: get("sha256")}
		if v, _ := p.get(rstr("data")); v == rbool(true) {
			patch.Data = f.b.data
		}
		out = append(out, patch)
	}
	return out
}

type resourceRef struct{ r *spec }

func (resourceRef) rubyValue() {}

var stageCount atomic.Int64

func (b *builder) stage(r resourceRef) string {
	archive, ok := b.env.Resources[r.r.name]
	if !ok || b.env.Unpack == nil {
		failBuild("resource %s was not downloaded", r.r.name)
	}
	dir := filepath.Join(b.env.Scratch, "resource-"+r.r.name+"-"+strconv.FormatInt(stageCount.Add(1), 10))
	b.must(b.env.Host.MkdirAll(dir))
	b.must(b.env.Unpack(archive, dir))
	entries, err := b.env.Host.ReadDir(dir)
	b.must(err)
	if len(entries) == 1 && entries[0].IsDir() {
		return filepath.Join(dir, entries[0].Name())
	}
	return dir
}

func (b *builder) installResource(dir string, r resourceRef) {
	staged := b.stage(r)
	entries, err := b.env.Host.ReadDir(staged)
	b.must(err)
	b.must(b.env.Host.MkdirAll(dir))
	for _, e := range entries {
		b.must(b.env.Host.Rename(filepath.Join(staged, e.Name()), filepath.Join(dir, e.Name())))
	}
}

func (b *builder) resourceMethod(in *interp, r resourceRef, name string, args []value, blk *proc) value {
	switch name {
	case "name":
		return rstr(r.r.name)
	case "url":
		return rstr(r.r.url)
	case "version":
		return ver(r.r.detectedVersion())
	case "stage":
		if len(args) == 1 {
			b.installResource(b.abs(str(args[0])), r)
			return nil
		}
		if blk == nil {
			fail("resource stage without a target or block")
		}
		prev := b.cwd
		b.cwd = b.stage(r)
		defer func() { b.cwd = prev }()
		return in.callBlock(blk)
	}
	fail("resource.%s", name)
	return nil
}

func (b *builder) resource(name string) resourceRef {
	if sp := b.f.active(); sp != nil {
		for _, r := range sp.resource {
			if r.name == name {
				return resourceRef{r}
			}
		}
	}
	panic(buildFailure{fmt.Errorf("no resource %q", name)})
}
