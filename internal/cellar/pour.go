package cellar

import (
	"bytes"
	"encoding/json"
	"time"
)

type Pour struct {
	OnRequest bool
	Time      time.Time
	Aliases   []string
	Stable    string // the formula's version, without its revision
	Scheme    int
	Tap       string
	Path      string // where the API data brew would have read from lives, or a tap's formula file
	// TapGitHead is the commit of the tap a tap formula came from; empty for the API's.
	TapGitHead string
	Arch       string // the CPU of this host, "arm64" or "x86_64"; empty keeps the bottle's
}

// Tab keys we do not model are dropped, as brew drops them.
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
	if p.TapGitHead != "" {
		no := false
		r.LoadedFromAPI, r.LoadedFromInternalAPI = false, &no
	}
	r.InstalledOnRequest = p.OnRequest
	r.InstalledAsDependency = nil
	r.Time = p.Time.Unix()
	if p.Arch != "" {
		r.Arch = p.Arch
	}
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

func pourSource(p Pour, scm string) (Source, error) {
	type versions struct {
		Stable               string  `json:"stable"`
		Head                 *string `json:"head"`
		VersionScheme        int     `json:"version_scheme"`
		CompatibilityVersion *int    `json:"compatibility_version"`
	}
	var head *string
	if p.TapGitHead != "" {
		head = &p.TapGitHead
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
	}{scm, "stable", versions{Stable: p.Stable, VersionScheme: p.Scheme}, p.Path, head, p.Tap})
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
