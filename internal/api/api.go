// Package api reads Homebrew's formula metadata from formulae.brew.sh.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const DefaultURL = "https://formulae.brew.sh/api/formula.json"

type Formula struct {
	Name                 string    `json:"name"`
	FullName             string    `json:"full_name"`
	Desc                 string    `json:"desc"`
	Homepage             string    `json:"homepage"`
	License              string    `json:"license"`
	Caveats              string    `json:"caveats"`
	RubySourcePath       string    `json:"ruby_source_path"`
	Deprecated           bool      `json:"deprecated"`
	Disabled             bool      `json:"disabled"`
	DeprecationReason    string    `json:"deprecation_reason"`
	DeprecationDate      string    `json:"deprecation_date"`
	DisableReason        string    `json:"disable_reason"`
	DisableDate          string    `json:"disable_date"`
	ConflictsWith        []string  `json:"conflicts_with"`
	ConflictsWithReasons []*string `json:"conflicts_with_reasons"`

	DeprecationReplacementFormula string                        `json:"deprecation_replacement_formula"`
	DeprecationReplacementCask    string                        `json:"deprecation_replacement_cask"`
	DisableReplacementFormula     string                        `json:"disable_replacement_formula"`
	DisableReplacementCask        string                        `json:"disable_replacement_cask"`
	VersionedFormulae             []string                      `json:"versioned_formulae"`
	Requirements                  []struct{ Name string }       `json:"requirements"`
	KegOnlyReason                 KegOnlyReason                 `json:"keg_only_reason"`
	Tap                           string                        `json:"tap"`
	Aliases                       []string                      `json:"aliases"`
	OldNames                      []string                      `json:"oldnames"`
	Versions                      struct{ Stable, Head string } `json:"versions"`
	Revision                      int                           `json:"revision"`
	VersionScheme                 int                           `json:"version_scheme"`
	Bottle                        struct{ Stable Bottle }       `json:"bottle"`
	Dependencies                  []string                      `json:"dependencies"`
	BuildDependencies             []string                      `json:"build_dependencies"`
	RecommendedDependencies       []string                      `json:"recommended_dependencies"`
	OptionalDependencies          []string                      `json:"optional_dependencies"`
	UsesFromMacOS                 []UsesFromMacOS               `json:"uses_from_macos"`
	UsesBounds                    []struct{ Since string }      `json:"uses_from_macos_bounds"`
	KegOnly                       bool                          `json:"keg_only"`
	PostInstallSteps              []json.RawMessage             `json:"post_install_steps"`
	Variations                    map[string]json.RawMessage    `json:"variations"`
}

// KegOnlyReason is why a formula is not linked. Reason is a symbol such as
// ":versioned_formula" or free text.
type KegOnlyReason struct {
	Reason      string `json:"reason"`
	Explanation string `json:"explanation"`
}

// UsesFromMacOS is a dependency macOS provides itself. It is a plain name or
// a one-entry object mapping the name to a tag or list of tags.
type UsesFromMacOS struct {
	Name  string
	Tags  []string
	Since string // macOS release that provides it; older macOS needs the real dependency
}

func (u *UsesFromMacOS) UnmarshalJSON(b []byte) error {
	var name string
	if json.Unmarshal(b, &name) == nil {
		*u = UsesFromMacOS{Name: name}
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil || len(obj) != 1 {
		return fmt.Errorf("uses_from_macos entry %s: want a name or a one-entry object", b)
	}
	for name, raw := range obj {
		var tags []string
		var one string
		switch {
		case json.Unmarshal(raw, &one) == nil:
			tags = []string{one}
		case json.Unmarshal(raw, &tags) != nil:
			return fmt.Errorf("uses_from_macos tags %s: want a string or list", raw)
		}
		*u = UsesFromMacOS{Name: name, Tags: tags}
	}
	return nil
}

// Runtime is false for build-only and test-only entries.
func (u UsesFromMacOS) Runtime() bool {
	return !slices.Contains(u.Tags, "build") && !slices.Contains(u.Tags, "test")
}

// ForTag returns a copy with the variation for the exact bottle tag applied.
func (f Formula) ForTag(tag string) (Formula, error) {
	raw, ok := f.Variations[tag]
	if !ok {
		return f, nil
	}
	var o struct {
		Dependencies  *[]string        `json:"dependencies"`
		UsesFromMacOS *[]UsesFromMacOS `json:"uses_from_macos"`
		KegOnly       *bool            `json:"keg_only"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return f, fmt.Errorf("%s variation %s: %w", f.Name, tag, err)
	}
	if o.Dependencies != nil {
		f.Dependencies = *o.Dependencies
	}
	if o.UsesFromMacOS != nil {
		f.UsesFromMacOS = *o.UsesFromMacOS
	}
	if o.KegOnly != nil {
		f.KegOnly = *o.KegOnly
	}
	return f, nil
}

type Bottle struct {
	Rebuild int                   `json:"rebuild"`
	RootURL string                `json:"root_url"`
	Files   map[string]BottleFile `json:"files"`
}

type BottleFile struct {
	Cellar string `json:"cellar"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// BottleTags returns the tags in sorted order.
func (f *Formula) BottleTags() []string {
	tags := make([]string, 0, len(f.Bottle.Stable.Files))
	for tag := range f.Bottle.Stable.Files {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	return tags
}

func Parse(b []byte) ([]Formula, error) {
	var formulae []Formula
	if err := json.Unmarshal(b, &formulae); err != nil {
		return nil, err
	}
	for i := range formulae {
		f := &formulae[i]
		for j := range f.UsesFromMacOS {
			if j < len(f.UsesBounds) {
				f.UsesFromMacOS[j].Since = f.UsesBounds[j].Since
			}
		}
	}
	return formulae, nil
}

// Index finds formulae by name, alias or old name. Real names win over aliases.
type Index struct {
	byName map[string]*Formula
	all    []*Formula
}

func NewIndex(formulae []Formula) *Index {
	idx := &Index{byName: make(map[string]*Formula, len(formulae))}
	for i := range formulae {
		idx.byName[formulae[i].Name] = &formulae[i]
		idx.all = append(idx.all, &formulae[i])
	}
	for i := range formulae {
		for _, alt := range slices.Concat(formulae[i].Aliases, formulae[i].OldNames) {
			if _, taken := idx.byName[alt]; !taken {
				idx.byName[alt] = &formulae[i]
			}
		}
	}
	return idx
}

// Formulae returns every formula once, in listing order.
func (i *Index) Formulae() []*Formula { return i.all }

func (i *Index) Lookup(name string) (*Formula, bool) {
	f, ok := i.byName[name]
	return f, ok
}

// Client fetches the formula listing and caches it in CacheDir by ETag.
// When the fetch fails and a cache exists, the cache is used and Warn is called.
type Client struct {
	URL      string
	CacheDir string
	HTTP     *http.Client
	Warn     func(msg string)
}

func (c *Client) Formulae(ctx context.Context) ([]Formula, error) {
	body := filepath.Join(c.CacheDir, "formula.json")
	etagFile := filepath.Join(c.CacheDir, "formula.etag")
	cached, bodyErr := os.ReadFile(body)
	etag, etagErr := os.ReadFile(etagFile)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, err
	}
	if bodyErr == nil && etagErr == nil {
		req.Header.Set("If-None-Match", string(etag))
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return c.stale(cached, bodyErr, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if bodyErr != nil {
			return nil, fmt.Errorf("304 without a cached body: %w", bodyErr)
		}
		return Parse(cached)
	case http.StatusOK:
	default:
		return c.stale(cached, bodyErr, fmt.Errorf("GET %s: %s", c.URL, resp.Status))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	formulae, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(body, data); err != nil {
		return nil, err
	}
	if tag := resp.Header.Get("ETag"); tag != "" {
		err = writeAtomic(etagFile, []byte(tag))
	} else {
		err = os.Remove(etagFile) //fsx:allow cache file
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	return formulae, err
}

// Raw returns the cached JSON entry for the formula with this exact name,
// exactly as the API served it. Call Formulae first to fill the cache.
func (c *Client) Raw(name string) (json.RawMessage, error) {
	f, err := os.Open(filepath.Join(c.CacheDir, "formula.json"))
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	if _, err := dec.Token(); err != nil { // the opening bracket
		return nil, err
	}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		var probe struct{ Name string }
		if err := json.Unmarshal(raw, &probe); err != nil {
			return nil, err
		}
		if probe.Name == name {
			return raw, nil
		}
	}
	return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
}

// stale returns the cached listing after a failed fetch, or the fetch error if there is none.
func (c *Client) stale(cached []byte, bodyErr, fetchErr error) ([]Formula, error) {
	if bodyErr != nil {
		return nil, fetchErr
	}
	formulae, err := Parse(cached)
	if err != nil {
		return nil, fetchErr
	}
	if c.Warn != nil {
		c.Warn(fmt.Sprintf("using the cached formula list: %v", fetchErr))
	}
	return formulae, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //fsx:allow cache dir
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*") //fsx:allow cache file
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) //fsx:allow cache file
		return err
	}
	return os.Rename(tmp.Name(), path) //fsx:allow cache file
}
