package cellar

import (
	"bytes"
	"encoding/json"
	"time"
)

// BuiltOn is DevelopmentTools.build_system_info on macOS, in brew's key order.
// An empty Xcode is written as null, as brew writes a machine without it.
type BuiltOn struct {
	OS, OSVersion, CPUFamily, Xcode, CLT, PreferredPerl string
}

type Build struct {
	HomebrewVersion string
	OnRequest       bool
	Time            time.Time
	SourceModified  time.Time
	Aliases         []string
	Deps            []Dependency
	Tap, TapGitHead string // the tap's commit; empty writes null
	Path            string
	Stable          string
	Scheme          int
	Compatibility   int // 0 when the formula sets none
	Arch            string
	BuiltOn         BuiltOn
}

func orNull(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func encode(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// NewBuildReceipt mirrors brew's Tab.create.
func NewBuildReceipt(b Build) (*Receipt, error) {
	var compat *int
	if b.Compatibility != 0 {
		compat = &b.Compatibility
	}
	type versions struct {
		Stable               string  `json:"stable"`
		Head                 *string `json:"head"`
		VersionScheme        int     `json:"version_scheme"`
		CompatibilityVersion *int    `json:"compatibility_version"`
	}
	src, err := encode(struct {
		Tap        string   `json:"tap"`
		TapGitHead *string  `json:"tap_git_head"`
		Spec       string   `json:"spec"`
		Path       string   `json:"path"`
		Versions   versions `json:"versions"`
	}{b.Tap, orNull(b.TapGitHead), "stable", b.Path, versions{Stable: b.Stable, VersionScheme: b.Scheme, CompatibilityVersion: compat}})
	if err != nil {
		return nil, err
	}
	on, err := encode(struct {
		OS            string  `json:"os"`
		OSVersion     string  `json:"os_version"`
		CPUFamily     string  `json:"cpu_family"`
		Xcode         *string `json:"xcode"`
		CLT           *string `json:"clt"`
		PreferredPerl string  `json:"preferred_perl"`
	}{b.BuiltOn.OS, b.BuiltOn.OSVersion, b.BuiltOn.CPUFamily, orNull(b.BuiltOn.Xcode), orNull(b.BuiltOn.CLT), b.BuiltOn.PreferredPerl})
	if err != nil {
		return nil, err
	}
	no := false
	aliases, deps := b.Aliases, b.Deps
	if aliases == nil {
		aliases = []string{}
	}
	if deps == nil {
		deps = []Dependency{}
	}
	r := &Receipt{
		HomebrewVersion: b.HomebrewVersion, UsedOptions: []string{}, UnusedOptions: []string{},
		LoadedFromAPI: false, LoadedFromInternalAPI: &no, InstalledOnRequest: b.OnRequest,
		Time: b.Time.Unix(), SourceModifiedTime: b.SourceModified.Unix(), Compiler: "clang",
		Aliases: aliases, RuntimeDependencies: deps, Source: Source{Tap: b.Tap, scheme: b.Scheme, raw: src},
		Arch: b.Arch, BuiltOn: on, onRequestOK: true,
	}
	return r, nil
}
