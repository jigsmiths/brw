// Package api reads Homebrew's formula metadata from formulae.brew.sh.
package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
)

const DefaultURL = "https://formulae.brew.sh/api/formula.json"

type Formula struct {
	Name                 string   `json:"name"`
	FullName             string   `json:"full_name"`
	Desc                 string   `json:"desc"`
	Homepage             string   `json:"homepage"`
	License              string   `json:"license"`
	Caveats              string   `json:"caveats"`
	RubySourcePath       string   `json:"ruby_source_path"`
	Deprecated           bool     `json:"deprecated"`
	Disabled             bool     `json:"disabled"`
	DeprecationReason    string   `json:"deprecation_reason"`
	DeprecationDate      string   `json:"deprecation_date"`
	DisableReason        string   `json:"disable_reason"`
	DisableDate          string   `json:"disable_date"`
	ConflictsWith        []string `json:"conflicts_with"`
	ConflictsWithReasons []string `json:"conflicts_with_reasons"` // "" where the API has null

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

// Bottle lists the tags a formula has bottles for. The URL and checksum of each
// bottle are not kept; read them from the raw entry (Client.Raw).
type Bottle struct {
	Rebuild int
	RootURL string
	Tags    []string // sorted
}

func (b *Bottle) UnmarshalJSON(data []byte) error {
	var v struct {
		Rebuild int                 `json:"rebuild"`
		RootURL string              `json:"root_url"`
		Files   map[string]struct{} `json:"files"`
	}
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	*b = Bottle{Rebuild: v.Rebuild, RootURL: v.RootURL, Tags: slices.Sorted(maps.Keys(v.Files))}
	return nil
}

// BottleTags returns the tags in sorted order.
func (f *Formula) BottleTags() []string { return f.Bottle.Stable.Tags }

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

// freshFor is how long a validated listing is used without asking the server.
// The server allows ten minutes (Cache-Control: max-age=600).
const freshFor = 10 * time.Minute

// indexVersion changes when Parse starts building something different, so an
// index written by an older brw is not trusted. Changes to the Formula type
// itself are caught by its fingerprint.
const indexVersion = "1"

// Client fetches the formula listing and caches it in CacheDir by ETag. It also
// keeps an index of the parsed formulae, which is much faster to load than the
// listing. When the fetch fails and a cache exists, the cache is used and Warn
// is called.
type Client struct {
	URL      string
	CacheDir string
	HTTP     *http.Client
	Warn     func(msg string)
	Now      func() time.Time // defaults to time.Now
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Update says what Refresh found.
type Update struct {
	Changed bool
	Total   int
	// Added and Removed name the formulae that came or went. They are empty
	// when there was no listing before.
	Added, Removed []string
}

// Refresh asks the server for the listing even when the cached one is fresh.
// Unlike Formulae it fails when the server cannot be reached, rather than
// settling for the cache.
func (c *Client) Refresh(ctx context.Context) (Update, error) {
	before, _ := os.ReadFile(filepath.Join(c.CacheDir, "formula.json"))
	formulae, err := c.formulae(ctx, true)
	if err != nil {
		return Update{}, err
	}
	up := Update{Total: len(formulae)}
	after, err := os.ReadFile(filepath.Join(c.CacheDir, "formula.json"))
	if err != nil {
		return Update{}, err
	}
	if bytes.Equal(before, after) {
		return up, nil
	}
	up.Changed = true
	if old, err := Parse(before); err == nil && len(before) > 0 {
		up.Added, up.Removed = nameDiff(old, formulae)
	}
	return up, nil
}

// nameDiff lists the sorted names in next that are not in old, and the reverse.
func nameDiff(old, next []Formula) (added, removed []string) {
	was := map[string]bool{}
	for _, f := range old {
		was[f.Name] = true
	}
	for _, f := range next {
		if !was[f.Name] {
			added = append(added, f.Name)
		}
		delete(was, f.Name)
	}
	removed = slices.Sorted(maps.Keys(was))
	slices.Sort(added)
	return added, removed
}

func (c *Client) Formulae(ctx context.Context) ([]Formula, error) {
	return c.formulae(ctx, false)
}

// formulae loads the listing. With force it skips the freshness shortcut and
// reports a failed fetch instead of using the cache.
func (c *Client) formulae(ctx context.Context, force bool) ([]Formula, error) {
	body := filepath.Join(c.CacheDir, "formula.json")
	etagFile := filepath.Join(c.CacheDir, "formula.etag")
	etagBytes, etagErr := os.ReadFile(etagFile)
	etag := string(etagBytes)
	_, bodyErr := os.Stat(body)
	cached := bodyErr == nil && etagErr == nil

	if cached && !force && c.fresh(etagFile) {
		if formulae, ok := c.loadIndex(etag); ok {
			return formulae, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, err
	}
	if cached {
		req.Header.Set("If-None-Match", etag)
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return c.stale(etag, bodyErr == nil && !force, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if !cached {
			return nil, errors.New("304 without a cached listing")
		}
		formulae, err := c.fromCache(etag)
		if err != nil {
			return nil, err
		}
		c.touch(etagFile)
		return formulae, nil
	case http.StatusOK:
	default:
		return c.stale(etag, bodyErr == nil && !force, fmt.Errorf("GET %s: %s", c.URL, resp.Status))
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
	tag := resp.Header.Get("ETag")
	if tag != "" {
		err = writeAtomic(etagFile, []byte(tag))
		c.touch(etagFile)
		c.writeIndex(tag, formulae)
	} else {
		err = os.Remove(etagFile) //fsx:allow cache file
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	return formulae, err
}

// fresh reports whether the listing was validated recently enough to use as is.
func (c *Client) fresh(etagFile string) bool {
	fi, err := os.Stat(etagFile)
	if err != nil {
		return false
	}
	age := c.now().Sub(fi.ModTime())
	return age >= 0 && age < freshFor
}

// touch records that the cached listing was just validated.
func (c *Client) touch(etagFile string) {
	now := c.now()
	_ = os.Chtimes(etagFile, now, now) //fsx:allow cache file; a failure only costs a revalidation
}

// fromCache loads the cached listing, preferring the index and rebuilding it
// from the listing when it is unusable.
func (c *Client) fromCache(etag string) ([]Formula, error) {
	if formulae, ok := c.loadIndex(etag); ok {
		return formulae, nil
	}
	data, err := os.ReadFile(filepath.Join(c.CacheDir, "formula.json"))
	if err != nil {
		return nil, err
	}
	formulae, err := Parse(data)
	if err != nil {
		return nil, err
	}
	c.writeIndex(etag, formulae)
	return formulae, nil
}

// listingIndex is the parsed listing as stored on disk.
type listingIndex struct {
	Fingerprint string
	ETag        string
	Formulae    []Formula
}

var indexFingerprint = indexVersion + fingerprint(reflect.TypeFor[Formula]())

// loadIndex returns the index if it was built for this listing by a brw that
// parses the same way.
func (c *Client) loadIndex(etag string) ([]Formula, bool) {
	if etag == "" {
		return nil, false
	}
	f, err := os.Open(filepath.Join(c.CacheDir, "formula.index"))
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	var idx listingIndex
	if err := gob.NewDecoder(bufio.NewReader(f)).Decode(&idx); err != nil {
		return nil, false
	}
	if idx.Fingerprint != indexFingerprint || idx.ETag != etag {
		return nil, false
	}
	return idx.Formulae, true
}

// writeIndex is best effort: without an index the listing is parsed each time.
func (c *Client) writeIndex(etag string, formulae []Formula) {
	if etag == "" {
		return
	}
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(listingIndex{indexFingerprint, etag, formulae}); err != nil {
		return
	}
	_ = writeAtomic(filepath.Join(c.CacheDir, "formula.index"), b.Bytes())
}

// fingerprint summarises the shape of t, so that adding, removing or retyping a
// field changes it.
func fingerprint(t reflect.Type) string {
	var b strings.Builder
	describe(&b, t, map[reflect.Type]bool{})
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:8])
}

func describe(b *strings.Builder, t reflect.Type, seen map[reflect.Type]bool) {
	switch t.Kind() {
	case reflect.Struct:
		if seen[t] {
			b.WriteString("~" + t.Name())
			return
		}
		seen[t] = true
		b.WriteString("struct{")
		for i := range t.NumField() {
			f := t.Field(i)
			b.WriteString(f.Name + " ")
			describe(b, f.Type, seen)
			b.WriteString(";")
		}
		b.WriteString("}")
	case reflect.Slice, reflect.Array, reflect.Pointer:
		b.WriteString(t.Kind().String() + " ")
		describe(b, t.Elem(), seen)
	case reflect.Map:
		b.WriteString("map[")
		describe(b, t.Key(), seen)
		b.WriteString("]")
		describe(b, t.Elem(), seen)
	default:
		b.WriteString(t.String())
	}
}

// each calls fn with every entry of the cached listing, in order, until fn
// returns true.
func (c *Client) each(fn func(name string, raw json.RawMessage) (done bool, err error)) error {
	f, err := os.Open(filepath.Join(c.CacheDir, "formula.json"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	if _, err := dec.Token(); err != nil { // the opening bracket
		return err
	}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		var probe struct{ Name string }
		if err := json.Unmarshal(raw, &probe); err != nil {
			return err
		}
		if done, err := fn(probe.Name, raw); done || err != nil {
			return err
		}
	}
	return nil
}

// Raw returns the cached JSON entry for the formula with this exact name,
// exactly as the API served it. Call Formulae first to fill the cache.
func (c *Client) Raw(name string) (json.RawMessage, error) {
	var found json.RawMessage
	err := c.each(func(n string, raw json.RawMessage) (bool, error) {
		if n == name {
			found = raw
		}
		return found != nil, nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
	}
	return found, nil
}

// BottleFile says where one bottle is and what it must hash to.
type BottleFile struct {
	Cellar string `json:"cellar"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Bottles looks up the bottle of each formula for its tag, in one pass over the
// cached listing. want maps a formula name to a bottle tag.
func (c *Client) Bottles(want map[string]string) (map[string]BottleFile, error) {
	out := make(map[string]BottleFile, len(want))
	if len(want) == 0 {
		return out, nil
	}
	err := c.each(func(name string, raw json.RawMessage) (bool, error) {
		tag, ok := want[name]
		if !ok {
			return false, nil
		}
		var e struct {
			Bottle struct {
				Stable struct {
					Files map[string]BottleFile `json:"files"`
				} `json:"stable"`
			} `json:"bottle"`
		}
		if err := json.Unmarshal(raw, &e); err != nil {
			return false, fmt.Errorf("%s: %w", name, err)
		}
		file, ok := e.Bottle.Stable.Files[tag]
		if !ok {
			return false, fmt.Errorf("%s has no bottle for %s", name, tag)
		}
		out[name] = file
		return len(out) == len(want), nil
	})
	if err != nil {
		return nil, err
	}
	for name := range want {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
	}
	return out, nil
}

// stale returns the cached listing after a failed fetch, or the fetch error if there is none.
func (c *Client) stale(etag string, haveBody bool, fetchErr error) ([]Formula, error) {
	if !haveBody {
		return nil, fetchErr
	}
	formulae, err := c.fromCache(etag)
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
