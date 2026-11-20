package api

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
)

// DefaultCaskURL is the cask listing as Homebrew signs it, which brw checks before use.
const DefaultCaskURL = "https://formulae.brew.sh/api/cask.jws.json"

// Cask is an entry of the cask listing. Artifacts stay as the API has them,
// since each kind of artifact has its own shape.
type Cask struct {
	Token      string   `json:"token"`
	FullToken  string   `json:"full_token"`
	OldTokens  []string `json:"old_tokens"`
	Tap        string   `json:"tap"`
	TapGitHead string   `json:"tap_git_head"`
	Names      []string `json:"name"`
	Desc       string   `json:"desc"`
	Homepage   string   `json:"homepage"`
	Version    string   `json:"version"` // "latest" for a cask without versions
	URL        string   `json:"url"`
	SHA256     string   `json:"sha256"` // "no_check" when the download is not checked
	// URLSpecs are options for fetching the URL, such as user_agent.
	URLSpecs map[string]json.RawMessage `json:"url_specs"`
	// Container says how to open the download, such as {"type":"naked"}; null by default.
	Container      json.RawMessage `json:"container"`
	AutoUpdates    bool            `json:"auto_updates"`
	Caveats        string          `json:"caveats"`
	CaveatsRosetta bool            `json:"caveats_rosetta"` // built for Intel, so Apple silicon needs Rosetta 2
	Languages      []string        `json:"languages"`
	RubySourcePath string          `json:"ruby_source_path"`

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
	Rename        []json.RawMessage          `json:"rename"` // files to rename after unpacking
	Variations    map[string]json.RawMessage `json:"variations"`
}

// CaskDependsOn is what a cask needs before it can be installed.
type CaskDependsOn struct {
	Formula []string          `json:"formula"`
	Cask    []string          `json:"cask"`
	MacOS   *MacOSRequirement `json:"macos"` // nil when the cask names none
	Arch    []struct {
		Type string `json:"type"` // arm or intel
		Bits int    `json:"bits"`
	} `json:"arch"`
}

// MacOSRequirement is a cask's depends_on macos, such as ">= 12". Brew's
// comparison is ">=" when the cask names no version.
type MacOSRequirement struct {
	Comparator string
	Versions   []string
}

// UnmarshalJSON reads the API's form, a map of one comparison to versions: {}
// for any macOS, {">=": ["12"]} or {"==": ["11", "12"]}.
func (m *MacOSRequirement) UnmarshalJSON(b []byte) error {
	var raw map[string][]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*m = MacOSRequirement{Comparator: ">="}
	for cmp, versions := range raw {
		m.Comparator, m.Versions = cmp, versions
	}
	return nil
}

// ParseCasks decodes the cask listing.
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
		Version   *string                     `json:"version"`
		URL       *string                     `json:"url"`
		SHA256    *string                     `json:"sha256"`
		URLSpecs  *map[string]json.RawMessage `json:"url_specs"`
		Container *json.RawMessage            `json:"container"`
		Caveats   *string                     `json:"caveats"`
		DependsOn *CaskDependsOn              `json:"depends_on"`
		Artifacts *[]json.RawMessage          `json:"artifacts"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return c, fmt.Errorf("%s variation %s: %w", c.Token, tag, err)
	}
	set(&c.Version, o.Version)
	set(&c.URL, o.URL)
	set(&c.SHA256, o.SHA256)
	set(&c.URLSpecs, o.URLSpecs)
	set(&c.Container, o.Container)
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
	byToken map[string]int // into all
	all     []*Cask

	// From Client.CaskIndex: the cached listing and where each cask's entry is
	// in it, for completing the casks that were loaded without their artifacts
	// and variations. Nil when every cask is whole.
	mu      sync.Mutex
	listing string
	spans   []span
	whole   []bool
}

// NewCaskIndex indexes casks, which it keeps pointers into.
func NewCaskIndex(casks []Cask) *CaskIndex {
	idx := &CaskIndex{byToken: make(map[string]int, len(casks))}
	for i := range casks {
		idx.byToken[casks[i].Token] = i
		idx.all = append(idx.all, &casks[i])
	}
	for i := range casks {
		for _, old := range casks[i].OldTokens {
			if _, taken := idx.byToken[old]; !taken {
				idx.byToken[old] = i
			}
		}
	}
	return idx
}

// Casks returns every cask once, in listing order. From Client.CaskIndex they
// can lack their artifacts and variations: Lookup returns a cask whole.
func (i *CaskIndex) Casks() []*Cask { return i.all }

// Lookup finds a cask by token or old token.
func (i *CaskIndex) Lookup(token string) (*Cask, bool) {
	n, ok := i.byToken[token]
	if !ok || !i.complete(n) {
		return nil, false
	}
	return i.all[n], true
}

// complete reads the rest of a cask from the cached listing, or from a full
// parse of it when it changed since the index was loaded.
func (i *CaskIndex) complete(n int) bool {
	if i.spans == nil {
		return true
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.whole[n] {
		return true
	}
	c := i.all[n]
	var whole Cask
	raw, err := readEntry(i.listing, i.spans[n])
	if err != nil || json.Unmarshal(raw, &whole) != nil || whole.Token != c.Token {
		data, err := os.ReadFile(i.listing)
		if err != nil {
			return false
		}
		casks, err := ParseCasks(data)
		k := slices.IndexFunc(casks, func(k Cask) bool { return k.Token == c.Token })
		if err != nil || k < 0 {
			return false
		}
		whole = casks[k]
	}
	*c, i.whole[n] = whole, true
	return true
}

// CaskIndex loads the cask listing, with the same caching as Formulae. Casks
// come from the cache without their artifacts and variations, which are most
// of the listing, and Lookup reads those for just the casks it is asked for.
func (c *Client) CaskIndex(ctx context.Context) (*CaskIndex, error) {
	var spans []span
	l := c.caskListing()
	l.spans = &spans
	casks, err := load(ctx, c, l, false)
	if err != nil {
		return nil, err
	}
	idx := NewCaskIndex(casks)
	if spans != nil && len(spans) == len(casks) {
		idx.listing, idx.spans, idx.whole = l.file(c.CacheDir, ".json"), spans, make([]bool, len(casks))
	}
	return idx, nil
}

func (c *Client) caskListing() listing[Cask] {
	return listing[Cask]{stem: "cask", url: c.CaskURL, parse: ParseCasks, fingerprint: caskIndexFingerprint, slim: slimCask}
}

// slimCask leaves out what CaskIndex.Lookup reads back from the listing.
func slimCask(c Cask) Cask {
	c.Artifacts, c.Variations = nil, nil
	return c
}
