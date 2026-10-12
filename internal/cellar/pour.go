package cellar

import (
	"bytes"
	"encoding/json"
	"time"
)

// Pour is what brew adds to a bottle's receipt data when it pours the bottle.
type Pour struct {
	OnRequest bool
	Time      time.Time
	Aliases   []string
	Stable    string // the formula's version, without its revision
	Scheme    int
	Tap       string
	Path      string // where the API data brew would have read from lives
}

// NewPourReceipt builds a poured bottle's receipt like brew: the manifest tab
// gives the build details and brew overwrites ten fields. Tab keys we do not
// model are dropped, as brew drops them.
func NewPourReceipt(tab []byte, p Pour) (*Receipt, error) {
	r, err := ParseReceipt(tab)
	if err != nil {
		return nil, err
	}
	r.unknown = nil

	yes := true
	r.UsedOptions, r.UnusedOptions = []string{}, []string{}
	r.BuiltAsBottle, r.PouredFromBottle = true, true
	r.LoadedFromAPI, r.LoadedFromInternalAPI = true, &yes
	r.InstalledOnRequest = p.OnRequest
	r.InstalledAsDependency = nil
	r.Time = p.Time.Unix()
	r.Aliases = p.Aliases
	if r.Aliases == nil {
		r.Aliases = []string{}
	}
	for i := range r.RuntimeDependencies {
		r.RuntimeDependencies[i].CompatibilityVersion = nil
	}
	src, err := pourSource(p, r.Source.scmRevision())
	if err != nil {
		return nil, err
	}
	r.Source = src
	return r, nil
}

// pourSource is brew's source block for an API formula, in brew's key order.
func pourSource(p Pour, scm string) (Source, error) {
	type versions struct {
		Stable               string  `json:"stable"`
		Head                 *string `json:"head"`
		VersionScheme        int     `json:"version_scheme"`
		CompatibilityVersion *int    `json:"compatibility_version"`
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	err := enc.Encode(struct {
		SCMRevision string   `json:"scm_revision,omitempty"`
		Spec        string   `json:"spec"`
		Versions    versions `json:"versions"`
		Path        string   `json:"path"`
		TapGitHead  *string  `json:"tap_git_head"`
		Tap         string   `json:"tap"`
	}{scm, "stable", versions{Stable: p.Stable, VersionScheme: p.Scheme}, p.Path, nil, p.Tap})
	if err != nil {
		return Source{}, err
	}
	return Source{Tap: p.Tap, scheme: p.Scheme, raw: bytes.TrimSuffix(buf.Bytes(), []byte("\n"))}, nil
}

// scmRevision is the commit the bottle was built from, which brew keeps.
func (s Source) scmRevision() string {
	var v struct {
		SCMRevision string `json:"scm_revision"`
	}
	_ = json.Unmarshal(s.raw, &v)
	return v.SCMRevision
}
