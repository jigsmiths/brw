// Package api reads Homebrew's formula metadata from formulae.brew.sh.
package api

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rsa"
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

const DefaultURL = "https://formulae.brew.sh/api/formula.jws.json"

type Requirement struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	Contexts []string `json:"contexts"` // "build" for a build-time requirement
	Specs    []string `json:"specs"`    // "stable", "head"
}

// ForOS: brew's name search leaves out a formula that needs the other OS.
func (f *Formula) ForOS(goos string) bool {
	for _, r := range f.Requirements {
		if slices.Contains(r.Contexts, "build") {
			continue
		}
		if r.Name == "linux" && goos != "linux" || (r.Name == "macos" || r.Name == "maximum_macos") && goos == "linux" {
			return false
		}
	}
	return true
}

type Formula struct {
	Name           string `json:"name"`
	FullName       string `json:"full_name"`
	Desc           string `json:"desc"`
	Homepage       string `json:"homepage"`
	License        string `json:"license"`
	Caveats        string `json:"caveats"`
	RubySourcePath string `json:"ruby_source_path"`
	TapGitHead     string `json:"tap_git_head"`
	URLs           struct {
		Stable struct {
			URL      string `json:"url"`
			Checksum string `json:"checksum"`
		} `json:"stable"`
		Head struct {
			URL    string `json:"url"`
			Branch string `json:"branch"`
			Using  string `json:"using"` // a strategy such as "git"; empty to detect it from the URL
		} `json:"head"`
	} `json:"urls"`
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
	Requirements                  []Requirement                 `json:"requirements"`
	KegOnlyReason                 KegOnlyReason                 `json:"keg_only_reason"`
	Tap                           string                        `json:"tap"`
	Aliases                       []string                      `json:"aliases"`
	OldNames                      []string                      `json:"oldnames"`
	Versions                      struct{ Stable, Head string } `json:"versions"`
	Revision                      int                           `json:"revision"`
	VersionScheme                 int                           `json:"version_scheme"`
	CompatibilityVersion          int                           `json:"compatibility_version"` // 0 when unset
	Bottle                        struct{ Stable Bottle }       `json:"bottle"`
	Dependencies                  []string                      `json:"dependencies"`
	BuildDependencies             []string                      `json:"build_dependencies"`
	RecommendedDependencies       []string                      `json:"recommended_dependencies"`
	OptionalDependencies          []string                      `json:"optional_dependencies"`
	UsesFromMacOS                 []UsesFromMacOS               `json:"uses_from_macos"`
	UsesBounds                    []struct{ Since string }      `json:"uses_from_macos_bounds"`
	KegOnly                       bool                          `json:"keg_only"`
	PostInstallSteps              []json.RawMessage             `json:"post_install_steps"`
	PostInstallDefined            bool                          `json:"post_install_defined"`
	Variations                    map[string]json.RawMessage    `json:"variations"`
	// HeadDependencies are the head spec's, when they differ from stable's.
	HeadDependencies HeadDeps `json:"head_dependencies"`
}

// HeadDeps are a head spec's dependencies; Set says the listing had them.
type HeadDeps struct {
	Set                     bool                     `json:"-"`
	Dependencies            []string                 `json:"dependencies"`
	BuildDependencies       []string                 `json:"build_dependencies"`
	RecommendedDependencies []string                 `json:"recommended_dependencies"`
	OptionalDependencies    []string                 `json:"optional_dependencies"`
	UsesFromMacOS           []UsesFromMacOS          `json:"uses_from_macos"`
	UsesBounds              []struct{ Since string } `json:"uses_from_macos_bounds"`
}

func (h *HeadDeps) UnmarshalJSON(b []byte) error {
	type plain HeadDeps
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*h = HeadDeps(p)
	h.Set = true
	return nil
}

// HeadSpec is f as install --HEAD sees it: with the head spec's dependencies.
func (f Formula) HeadSpec() Formula {
	if h := f.HeadDependencies; h.Set {
		f.Dependencies, f.BuildDependencies = h.Dependencies, h.BuildDependencies
		f.RecommendedDependencies, f.OptionalDependencies = h.RecommendedDependencies, h.OptionalDependencies
		f.UsesFromMacOS, f.UsesBounds = slices.Clone(h.UsesFromMacOS), h.UsesBounds
		for j := range f.UsesFromMacOS {
			if j < len(f.UsesBounds) {
				f.UsesFromMacOS[j].Since = f.UsesBounds[j].Since
			}
		}
		f.Variations = nil // a tap formula is evaluated for this host, so they repeat stable's
	}
	return f
}

// Reason is a symbol such as ":versioned_formula" or free text.
type KegOnlyReason struct {
	Reason      string `json:"reason"`
	Explanation string `json:"explanation"`
}

// UsesFromMacOS is a plain name or a one-entry object mapping the name to a tag
// or list of tags.
type UsesFromMacOS struct {
	Name  string
	Tags  []string
	Since string // older macOS needs the real dependency
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

func (u UsesFromMacOS) Runtime() bool {
	return !slices.Contains(u.Tags, "build") && !slices.Contains(u.Tags, "test")
}

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

// Bottle keeps no URL or checksum; read them from Client.Raw.
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
// The API's formulae win over a tap's for a bare name.
type Index struct {
	byName map[string]*Formula
	all    []*Formula

	tapFull  map[string]*tapFormula   // tap/name, tap/alias
	tapBare  map[string][]*tapFormula // name or alias, in the taps that have it
	tapOrder []*tapFormula
	skipped  []string
	onSkip   func(tap string)
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

func (i *Index) Formulae() []*Formula {
	if len(i.tapOrder) == 0 && len(i.skipped) == 0 {
		return i.all
	}
	return append(slices.Clip(i.all), i.tapFormulae()...)
}

// Lookup finds a formula by name, alias, old name or full name, such as
// fluxcd/tap/flux.
func (i *Index) Lookup(name string) (*Formula, bool) {
	if parts := strings.Split(name, "/"); len(parts) == 3 {
		return i.LookupFrom(parts[0]+"/"+parts[1], parts[2])
	}
	if f, ok := i.byName[name]; ok {
		return f, true
	}
	return i.lookupTap(name)
}

const freshFor = 10 * time.Minute

// indexVersion is bumped when parsing or the index shape changes, so an older
// brw's index is not trusted. Type changes are caught by fingerprints.
const indexVersion = "4"

// A failed fetch falls back to the cache and calls Warn.
type Client struct {
	URL      string
	CaskURL  string
	CacheDir string
	Key      *rsa.PublicKey // Homebrew's own key when nil
	HTTP     *http.Client
	Warn     func(msg string)
	Now      func() time.Time // defaults to time.Now

	entries map[string]span // set by Formulae
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

type Update struct {
	Changed bool
	Total   int
	// Added and Removed are empty when there was no listing before.
	Added, Removed []string
}

// Refresh fetches the listing even when the cache is fresh, and unlike Formulae
// fails rather than falling back to the cache.
func (c *Client) Refresh(ctx context.Context) (Update, error) {
	return refresh(ctx, c, c.formulaListing(), func(f Formula) string { return f.Name })
}

// RefreshCasks is Refresh for the cask listing.
func (c *Client) RefreshCasks(ctx context.Context) (Update, error) {
	return refresh(ctx, c, c.caskListing(), func(k Cask) string { return k.Token })
}

func refresh[T any](ctx context.Context, c *Client, l listing[T], name func(T) string) (Update, error) {
	before, _ := os.ReadFile(l.file(c.CacheDir, ".json"))
	items, err := load(ctx, c, l, true)
	if err != nil {
		return Update{}, err
	}
	up := Update{Total: len(items)}
	after, err := os.ReadFile(l.file(c.CacheDir, ".json"))
	if err != nil {
		return Update{}, err
	}
	if bytes.Equal(before, after) {
		return up, nil
	}
	up.Changed = true
	if old, err := l.parse(before); err == nil && len(before) > 0 {
		up.Added, up.Removed = nameDiff(old, items, name)
	}
	return up, nil
}

func nameDiff[T any](old, next []T, name func(T) string) (added, removed []string) {
	was := map[string]bool{}
	for _, item := range old {
		was[name(item)] = true
	}
	for _, item := range next {
		if n := name(item); !was[n] {
			added = append(added, n)
		}
		delete(was, name(item))
	}
	removed = slices.Sorted(maps.Keys(was))
	slices.Sort(added)
	return added, removed
}

// Formulae records entry spans so Raw and Bottles need no scan.
func (c *Client) Formulae(ctx context.Context) ([]Formula, error) {
	var spans []span
	l := c.formulaListing()
	l.spans = &spans
	items, err := load(ctx, c, l, false)
	if err != nil {
		return nil, err
	}
	c.entries = nil
	if len(spans) == len(items) {
		c.entries = make(map[string]span, len(items))
		for i := range items {
			c.entries[items[i].Name] = spans[i]
		}
	}
	return items, nil
}

// listing is cached in CacheDir as <stem>.json, <stem>.etag and <stem>.index.
type listing[T any] struct {
	stem        string
	url         string
	parse       func([]byte) ([]T, error)
	fingerprint string  // see indexFingerprint
	spans       *[]span // filled when not nil
	// slim drops what an entry's span can supply; used only when spans are known.
	slim func(T) T
}

func (l listing[T]) file(dir, ext string) string { return filepath.Join(dir, l.stem+ext) }

func (c *Client) formulaListing() listing[Formula] {
	return listing[Formula]{stem: "formula", url: c.URL, parse: Parse, fingerprint: indexFingerprint}
}

// With force, load skips the freshness shortcut and fails rather than using the cache.
func load[T any](ctx context.Context, c *Client, l listing[T], force bool) ([]T, error) {
	body := l.file(c.CacheDir, ".json")
	etagFile := l.file(c.CacheDir, ".etag")
	etagBytes, etagErr := os.ReadFile(etagFile)
	etag := string(etagBytes)
	_, bodyErr := os.Stat(body)
	cached := bodyErr == nil && etagErr == nil

	if cached && !force && c.fresh(etagFile) {
		if items, ok := loadIndex(c, l, etag); ok {
			return items, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.url, nil)
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
		return stale(c, l, etag, bodyErr == nil && !force, err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if !cached {
			return nil, errors.New("304 without a cached listing")
		}
		items, err := fromCache(c, l, etag)
		if err != nil {
			return nil, err
		}
		c.touch(etagFile)
		return items, nil
	case http.StatusOK:
	default:
		return stale(c, l, etag, bodyErr == nil && !force, fmt.Errorf("GET %s: %s", l.url, resp.Status))
	}

	signed, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	key := c.Key
	if key == nil {
		if key, err = homebrewKey(); err != nil {
			return nil, err
		}
	}
	data, err := verify(signed, key)
	if err != nil {
		return nil, fmt.Errorf("the %s listing from %s: %w", l.stem, l.url, err)
	}
	items, err := l.parse(data)
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
		storeIndex(c, l, tag, items, data)
	} else {
		err = os.Remove(etagFile) //fsx:allow cache file
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	return items, err
}

func (c *Client) fresh(etagFile string) bool {
	fi, err := os.Stat(etagFile)
	if err != nil {
		return false
	}
	age := c.now().Sub(fi.ModTime())
	return age >= 0 && age < freshFor
}

func (c *Client) touch(etagFile string) {
	now := c.now()
	_ = os.Chtimes(etagFile, now, now) //fsx:allow cache file; a failure only costs a revalidation
}

func fromCache[T any](c *Client, l listing[T], etag string) ([]T, error) {
	if items, ok := loadIndex(c, l, etag); ok {
		return items, nil
	}
	data, err := os.ReadFile(l.file(c.CacheDir, ".json"))
	if err != nil {
		return nil, err
	}
	items, err := l.parse(data)
	if err != nil {
		return nil, err
	}
	storeIndex(c, l, etag, items, data)
	return items, nil
}

type listingIndex[T any] struct {
	Fingerprint string
	ETag        string
	Items       []T
	Spans       []span // nil if unknown
}

type span struct{ Off, Len int64 }

func entrySpans(data []byte) ([]span, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	var spans []span
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		n := int64(len(raw))
		spans = append(spans, span{dec.InputOffset() - n, n})
	}
	return spans, nil
}

var (
	indexFingerprint     = indexVersion + fingerprint(reflect.TypeFor[Formula]())
	caskIndexFingerprint = indexVersion + fingerprint(reflect.TypeFor[Cask]())
)

// loadIndex returns the index if it was built for this listing by a brw that
// parses the same way.
func loadIndex[T any](c *Client, l listing[T], etag string) ([]T, bool) {
	if etag == "" {
		return nil, false
	}
	f, err := os.Open(l.file(c.CacheDir, ".index"))
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	var idx listingIndex[T]
	if err := gob.NewDecoder(bufio.NewReader(f)).Decode(&idx); err != nil {
		return nil, false
	}
	if idx.Fingerprint != l.fingerprint || idx.ETag != etag {
		return nil, false
	}
	if l.spans != nil {
		*l.spans = idx.Spans
	}
	return idx.Items, true
}

// storeIndex is best effort; data is what the spans point into.
func storeIndex[T any](c *Client, l listing[T], etag string, items []T, data []byte) {
	if etag == "" {
		return
	}
	spans, err := entrySpans(data)
	if err != nil || len(spans) != len(items) {
		spans = nil
	}
	if l.spans != nil {
		*l.spans = spans
	}
	if l.slim != nil && spans != nil {
		slim := make([]T, len(items))
		for i, item := range items {
			slim[i] = l.slim(item)
		}
		items = slim
	}
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(listingIndex[T]{l.fingerprint, etag, items, spans}); err != nil {
		return
	}
	_ = writeAtomic(l.file(c.CacheDir, ".index"), b.Bytes())
}

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
		for f := range t.Fields() {
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

func (c *Client) each(fn func(name string, raw json.RawMessage) (done bool, err error)) error {
	f, err := os.Open(filepath.Join(c.CacheDir, "formula.json"))
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	dec := json.NewDecoder(f)
	if _, err := dec.Token(); err != nil {
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

func readEntry(file string, s span) ([]byte, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	raw := make([]byte, s.Len)
	if _, err := f.ReadAt(raw, s.Off); err != nil {
		return nil, err
	}
	return raw, nil
}

// entry is not ok when the span is unknown or the listing changed since.
func (c *Client) entry(name string) (json.RawMessage, bool) {
	s, ok := c.entries[name]
	if !ok {
		return nil, false
	}
	raw, err := readEntry(filepath.Join(c.CacheDir, "formula.json"), s)
	if err != nil {
		return nil, false
	}
	var probe struct{ Name string }
	if json.Unmarshal(raw, &probe) != nil || probe.Name != name {
		return nil, false
	}
	return raw, true
}

// Raw needs Formulae called first to fill the cache.
func (c *Client) Raw(name string) (json.RawMessage, error) {
	if raw, ok := c.entry(name); ok {
		return raw, nil
	}
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

type BottleFile struct {
	Cellar string `json:"cellar"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// Bottles scans the cached listing once for any entry Formulae did not locate.
func (c *Client) Bottles(want map[string]string) (map[string]BottleFile, error) {
	out := make(map[string]BottleFile, len(want))
	rest := map[string]string{}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		raw, ok := c.entry(name)
		if !ok {
			rest[name] = want[name]
			continue
		}
		file, err := bottleFile(name, want[name], raw)
		if err != nil {
			return nil, err
		}
		out[name] = file
	}
	if len(rest) > 0 {
		found := 0
		err := c.each(func(name string, raw json.RawMessage) (bool, error) {
			tag, ok := rest[name]
			if !ok {
				return false, nil
			}
			file, err := bottleFile(name, tag, raw)
			if err != nil {
				return false, err
			}
			out[name] = file
			found++
			return found == len(rest), nil
		})
		if err != nil {
			return nil, err
		}
	}
	for name := range want {
		if _, ok := out[name]; !ok {
			return nil, fmt.Errorf("%s: %w", name, fs.ErrNotExist)
		}
	}
	return out, nil
}

func (i *Index) TapBottle(fullName, tag string) (BottleFile, error) {
	raw, ok := i.TapRaw(fullName)
	if !ok {
		return BottleFile{}, fmt.Errorf("%s: %w", fullName, fs.ErrNotExist)
	}
	return bottleFile(fullName, tag, raw)
}

func bottleFile(name, tag string, raw json.RawMessage) (BottleFile, error) {
	var e struct {
		Bottle struct {
			Stable struct {
				Files map[string]BottleFile `json:"files"`
			} `json:"stable"`
		} `json:"bottle"`
	}
	if err := json.Unmarshal(raw, &e); err != nil {
		return BottleFile{}, fmt.Errorf("%s: %w", name, err)
	}
	file, ok := e.Bottle.Stable.Files[tag]
	if !ok {
		return BottleFile{}, fmt.Errorf("%s has no bottle for %s", name, tag)
	}
	return file, nil
}

func stale[T any](c *Client, l listing[T], etag string, haveBody bool, fetchErr error) ([]T, error) {
	if !haveBody {
		return nil, fetchErr
	}
	items, err := fromCache(c, l, etag)
	if err != nil {
		return nil, fetchErr
	}
	if c.Warn != nil {
		c.Warn(fmt.Sprintf("using the cached %s list: %v", l.stem, fetchErr))
	}
	return items, nil
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
