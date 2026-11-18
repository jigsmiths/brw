package api

import (
	"context"
	"encoding/json"
	"fmt"
)

// DefaultCaskURL is the cask listing as Homebrew signs it, which brw checks before use.
const DefaultCaskURL = "https://formulae.brew.sh/api/cask.jws.json"

// Cask is an entry of the cask listing. Artifacts stay as the API has them,
// since each kind of artifact has its own shape.
type Cask struct {
	Token          string   `json:"token"`
	FullToken      string   `json:"full_token"`
	OldTokens      []string `json:"old_tokens"`
	Tap            string   `json:"tap"`
	Names          []string `json:"name"`
	Desc           string   `json:"desc"`
	Homepage       string   `json:"homepage"`
	Version        string   `json:"version"` // "latest" for a cask without versions
	URL            string   `json:"url"`
	SHA256         string   `json:"sha256"` // "no_check" when the download is not checked
	AutoUpdates    bool     `json:"auto_updates"`
	Caveats        string   `json:"caveats"`
	RubySourcePath string   `json:"ruby_source_path"`

	Deprecated                    bool   `json:"deprecated"`
	Disabled                      bool   `json:"disabled"`
	DeprecationReason             string `json:"deprecation_reason"`
	DeprecationDate               string `json:"deprecation_date"`
	DisableReason                 string `json:"disable_reason"`
	DisableDate                   string `json:"disable_date"`
	DeprecationReplacementFormula string `json:"deprecation_replacement_formula"`
	DeprecationReplacementCask    string `json:"deprecation_replacement_cask"`
	DisableReplacementFormula     string `json:"disable_replacement_formula"`
	DisableReplacementCask        string `json:"disable_replacement_cask"`

	DependsOn     CaskDependsOn              `json:"depends_on"`
	ConflictsWith struct{ Cask []string }    `json:"conflicts_with"`
	Artifacts     []json.RawMessage          `json:"artifacts"`
	Variations    map[string]json.RawMessage `json:"variations"`
}

// CaskDependsOn is what a cask needs before it can be installed.
type CaskDependsOn struct {
	Formula []string `json:"formula"`
	Cask    []string `json:"cask"`
	// MacOS maps a comparison such as ">=" or "==" to macOS versions.
	MacOS map[string][]string `json:"macos"`
	Arch  []struct {
		Type string `json:"type"` // arm or intel
		Bits int    `json:"bits"`
	} `json:"arch"`
}

func ParseCasks(b []byte) ([]Cask, error) {
	var casks []Cask
	if err := json.Unmarshal(b, &casks); err != nil {
		return nil, err
	}
	return casks, nil
}

// ForTag returns a copy with the variation for the exact tag applied. A null in
// the variation keeps the cask's own value.
func (c Cask) ForTag(tag string) (Cask, error) {
	raw, ok := c.Variations[tag]
	if !ok {
		return c, nil
	}
	var o struct {
		Version   *string            `json:"version"`
		URL       *string            `json:"url"`
		SHA256    *string            `json:"sha256"`
		Caveats   *string            `json:"caveats"`
		DependsOn *CaskDependsOn     `json:"depends_on"`
		Artifacts *[]json.RawMessage `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return c, fmt.Errorf("%s variation %s: %w", c.Token, tag, err)
	}
	set(&c.Version, o.Version)
	set(&c.URL, o.URL)
	set(&c.SHA256, o.SHA256)
	set(&c.Caveats, o.Caveats)
	set(&c.DependsOn, o.DependsOn)
	set(&c.Artifacts, o.Artifacts)
	return c, nil
}

func set[T any](dst *T, v *T) {
	if v != nil {
		*dst = *v
	}
}

// CaskIndex finds casks by token or old token. Current tokens win over old ones.
type CaskIndex struct {
	byToken map[string]*Cask
	all     []*Cask
}

func NewCaskIndex(casks []Cask) *CaskIndex {
	idx := &CaskIndex{byToken: make(map[string]*Cask, len(casks))}
	for i := range casks {
		idx.byToken[casks[i].Token] = &casks[i]
		idx.all = append(idx.all, &casks[i])
	}
	for i := range casks {
		for _, old := range casks[i].OldTokens {
			if _, taken := idx.byToken[old]; !taken {
				idx.byToken[old] = &casks[i]
			}
		}
	}
	return idx
}

// Casks returns every cask once, in listing order.
func (i *CaskIndex) Casks() []*Cask { return i.all }

func (i *CaskIndex) Lookup(token string) (*Cask, bool) {
	c, ok := i.byToken[token]
	return c, ok
}

// Casks loads the cask listing, with the same caching as Formulae.
func (c *Client) Casks(ctx context.Context) ([]Cask, error) {
	return load(ctx, c, c.caskListing(), false)
}

func (c *Client) caskListing() listing[Cask] {
	return listing[Cask]{stem: "cask", url: c.CaskURL, parse: ParseCasks, fingerprint: caskIndexFingerprint}
}
