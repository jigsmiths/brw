package cellar

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// ErrUnknownKeys is returned by Marshal when a receipt has keys we don't model.
var ErrUnknownKeys = errors.New("receipt has unknown keys")

// Receipt is an INSTALL_RECEIPT.json. Field order matches brew's output.
type Receipt struct {
	HomebrewVersion       string          `json:"homebrew_version"`
	UsedOptions           []string        `json:"used_options"`
	UnusedOptions         []string        `json:"unused_options"`
	BuiltAsBottle         bool            `json:"built_as_bottle"`
	BuiltPrefix           string          `json:"built_prefix,omitempty"`
	PouredFromBottle      bool            `json:"poured_from_bottle"`
	LoadedFromAPI         bool            `json:"loaded_from_api"`
	LoadedFromInternalAPI *bool           `json:"loaded_from_internal_api,omitempty"`
	InstalledAsDependency *bool           `json:"installed_as_dependency,omitempty"`
	InstalledOnRequest    bool            `json:"installed_on_request"`
	ChangedFiles          []string        `json:"changed_files"`
	LinkageFiles          []string        `json:"linkage_files,omitzero"`
	BinaryRelocationFiles []string        `json:"binary_relocation_files,omitzero"`
	Time                  int64           `json:"time"`
	SourceModifiedTime    int64           `json:"source_modified_time"`
	Compiler              string          `json:"compiler"`
	Aliases               []string        `json:"aliases"`
	RuntimeDependencies   []Dependency    `json:"runtime_dependencies"`
	Source                Source          `json:"source"`
	Arch                  string          `json:"arch"`
	BuiltOn               json.RawMessage `json:"built_on"`

	unknown error
}

type Dependency struct {
	FullName             string          `json:"full_name"`
	Version              string          `json:"version"`
	Revision             int             `json:"revision"`
	BottleRebuild        int             `json:"bottle_rebuild"`
	PkgVersion           string          `json:"pkg_version"`
	DeclaredDirectly     bool            `json:"declared_directly"`
	CompatibilityVersion json.RawMessage `json:"compatibility_version,omitempty"`
}

// Source keeps its raw JSON because brew's key order varies.
type Source struct {
	Tap    string
	scheme int
	raw    json.RawMessage
}

func (s *Source) UnmarshalJSON(b []byte) error {
	var v struct {
		Tap      string `json:"tap"`
		Versions struct {
			Scheme int `json:"version_scheme"`
		} `json:"versions"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	s.Tap, s.scheme = v.Tap, v.Versions.Scheme
	s.raw = append(s.raw[:0], b...)
	return nil
}

func (s Source) MarshalJSON() ([]byte, error) {
	if s.raw == nil {
		return json.Marshal(map[string]string{"tap": s.Tap})
	}
	return s.raw, nil
}

// VersionScheme is the formula's version scheme when the keg was installed.
func (r *Receipt) VersionScheme() int { return r.Source.scheme }

func ParseReceipt(b []byte) (*Receipt, error) {
	var r Receipt
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var strict Receipt
	r.unknown = dec.Decode(&strict)
	return &r, nil
}

// Marshal writes brew's format: two-space indent, no trailing newline.
func (r *Receipt) Marshal() ([]byte, error) {
	if r.unknown != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnknownKeys, r.unknown)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
