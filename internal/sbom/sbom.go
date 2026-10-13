// Package sbom updates the software bill of materials that ships in a bottle,
// the way brew does when it pours the bottle.
package sbom

import (
	"time"
)

// File is the name of the SBOM in a keg.
const File = "sbom.spdx.json"

type Params struct {
	Created    time.Time // when the bottle was poured
	Creator    string    // who poured it, such as "Tool: https://github.com/jigsmiths/brw@abc1234"
	Supplement []byte    // the sh.brew.sbom.supplement annotation of the bottle's manifest, if any
	Tag        string    // the bottle tag, which picks the part of a per-tag supplement
}

// Update gives the SBOM of a bottle the creation time and creator of the pour and
// merges in the supplement, and reports whether it did. An SBOM that is not a
// JSON object with a creationInfo object is left alone, as brew leaves it.
func Update(data []byte, p Params) ([]byte, bool) {
	doc, err := parse(data)
	if err != nil {
		return data, false
	}
	info := doc.get("creationInfo")
	if !info.isObject() {
		return data, false
	}
	info.set("created", &node{kind: 's', text: p.Created.UTC().Format("2006-01-02T15:04:05Z")})
	info.set("creators", &node{kind: 'a', items: []*node{{kind: 's', text: p.Creator}}})
	if s := supplement(p.Supplement, p.Tag); s != nil {
		for _, key := range []string{"documentDescribes", "packages", "relationships"} {
			have, add := doc.get(key), s.get(key)
			if have.isArray() && add.isArray() {
				have.items = append(have.items, add.items...)
			}
		}
	}
	return doc.pretty(), true
}

// supplement picks what to merge: the whole annotation, or when it has a "tags"
// object, the part for this tag.
func supplement(raw []byte, tag string) *node {
	if len(raw) == 0 {
		return nil
	}
	s, err := parse(raw)
	if err != nil || !s.isObject() {
		return nil
	}
	if tags := s.get("tags"); tags.isObject() {
		return tags.get(tag)
	}
	return s
}
