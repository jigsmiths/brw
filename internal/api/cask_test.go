package api

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api/apitest"
)

const caskFixtureSize = 8

func caskFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "casks.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseCaskFixture(t *testing.T) []Cask {
	t.Helper()
	cs, err := ParseCasks(caskFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	return cs
}

func caskByToken(t *testing.T, token string) Cask {
	t.Helper()
	for _, c := range parseCaskFixture(t) {
		if c.Token == token {
			return c
		}
	}
	t.Fatalf("no cask %s in the fixture", token)
	return Cask{}
}

func TestParseCasks(t *testing.T) {
	cs := parseCaskFixture(t)
	if len(cs) != caskFixtureSize {
		t.Fatalf("got %d casks", len(cs))
	}
	slack := caskByToken(t, "slack")
	if slack.Tap != "homebrew/cask" || slack.Version != "4.53.292" || !slack.AutoUpdates || !slices.Equal(slack.Names, []string{"Slack"}) {
		t.Errorf("slack = %+v", slack)
	}
	if !slices.Equal(slack.ConflictsWith.Cask, []string{"slack@beta"}) || len(slack.Artifacts) == 0 || len(slack.SHA256) != 64 {
		t.Errorf("slack conflicts %v, %d artifacts, sha256 %q", slack.ConflictsWith.Cask, len(slack.Artifacts), slack.SHA256)
	}
	if cli := caskByToken(t, "1password-cli"); cli.AutoUpdates || cli.Caveats != "" {
		t.Errorf("nulls should parse as zero values: %+v", cli)
	}
	if n := caskByToken(t, "1password@nightly"); n.Version != "latest" || n.SHA256 != "no_check" {
		t.Errorf("1password@nightly version %q sha256 %q", n.Version, n.SHA256)
	}
	gcloud := caskByToken(t, "gcloud-cli")
	if !slices.Equal(gcloud.DependsOn.Formula, []string{"python@3.14"}) || !slices.Equal(gcloud.OldTokens, []string{"google-cloud-sdk"}) {
		t.Errorf("gcloud-cli depends on %+v, old tokens %v", gcloud.DependsOn, gcloud.OldTokens)
	}
	if m := caskByToken(t, "1password@nightly").DependsOn.MacOS; m == nil || m.Comparator != ">=" || !slices.Equal(m.Versions, []string{"12"}) {
		t.Errorf("1password@nightly macos = %+v", m)
	}
	// Any macOS at all, as against no requirement.
	if m := caskByToken(t, "slack").DependsOn.MacOS; m == nil || m.Comparator != ">=" || len(m.Versions) != 0 {
		t.Errorf("slack macos = %+v", m)
	}
	if m := caskByToken(t, "1password-cli").DependsOn.MacOS; m != nil {
		t.Errorf("1password-cli macos = %+v", m)
	}
	if b := caskByToken(t, "bridge"); !b.CaveatsRosetta {
		t.Error("bridge needs Rosetta")
	}
	if b := caskByToken(t, "bridge"); !b.Deprecated || b.DeprecationReplacementCask != "epic-games" {
		t.Errorf("bridge = %+v", b)
	}
}

func TestParseCasksInvalid(t *testing.T) {
	if _, err := ParseCasks([]byte("[{")); err == nil {
		t.Error("expected error")
	}
}

func forCaskTag(t *testing.T, c Cask, tag string) Cask {
	t.Helper()
	got, err := c.ForTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCaskForTag(t *testing.T) {
	slack := caskByToken(t, "slack")
	intel := forCaskTag(t, slack, "golden_gate")
	if !strings.Contains(intel.URL, "/x64/") || intel.SHA256 == slack.SHA256 || intel.Version != slack.Version {
		t.Errorf("intel slack = %s %s %s", intel.URL, intel.SHA256, intel.Version)
	}
	if got := forCaskTag(t, slack, "arm64_windows"); got.URL != slack.URL {
		t.Error("a tag without a variation should leave the cask as it is")
	}
	if c := caskByToken(t, "adobe-connect"); forCaskTag(t, c, "golden_gate").Version == c.Version {
		t.Error("the variation's version was not applied")
	}
	aptible := caskByToken(t, "aptible")
	if got := forCaskTag(t, aptible, "golden_gate"); slices.EqualFunc(got.Artifacts, aptible.Artifacts, func(a, b json.RawMessage) bool { return string(a) == string(b) }) {
		t.Error("the variation's artifacts were not applied")
	}
	if forCaskTag(t, slack, "golden_gate").Artifacts == nil {
		t.Error("a variation without artifacts should keep the cask's")
	}
}

func TestCaskForTagNullKeepsTheValue(t *testing.T) {
	c := Cask{Token: "x", Version: "1.0", SHA256: "abc", Variations: map[string]json.RawMessage{
		"t": json.RawMessage(`{"version": null, "sha256": null, "url": "https://example.com/x.dmg"}`),
	}}
	got := forCaskTag(t, c, "t")
	if got.Version != "1.0" || got.SHA256 != "abc" || got.URL != "https://example.com/x.dmg" {
		t.Errorf("got %+v", got)
	}
}

func TestCaskURLSpecsAndContainer(t *testing.T) {
	cs, err := ParseCasks([]byte(`[{"token": "x", "url_specs": {"user_agent": ":fake"}, "container": {"type": "naked"},
		"variations": {"t": {"url_specs": {}, "container": null}, "u": {"container": {"nested": "a.dmg"}}}}]`))
	if err != nil {
		t.Fatal(err)
	}
	c := cs[0]
	if string(c.URLSpecs["user_agent"]) != `":fake"` || string(c.Container) != `{"type": "naked"}` {
		t.Errorf("url_specs %v, container %s", c.URLSpecs, c.Container)
	}
	if got := forCaskTag(t, c, "t"); len(got.URLSpecs) != 0 || string(got.Container) != `{"type": "naked"}` {
		t.Errorf("t: url_specs %v, container %s", got.URLSpecs, got.Container)
	}
	if got := forCaskTag(t, c, "u"); string(got.Container) != `{"nested": "a.dmg"}` {
		t.Errorf("u: container %s", got.Container)
	}
}

func TestCaskForTagMalformedVariation(t *testing.T) {
	c := Cask{Token: "x", Variations: map[string]json.RawMessage{"t": json.RawMessage(`{"version": 3}`)}}
	if _, err := c.ForTag("t"); err == nil {
		t.Error("expected error")
	}
}

func TestCaskIndexLookup(t *testing.T) {
	cs := parseCaskFixture(t)
	idx := NewCaskIndex(cs)
	if len(idx.Casks()) != caskFixtureSize || idx.Casks()[0].Token != cs[0].Token {
		t.Errorf("Casks() = %d, want %d in listing order", len(idx.Casks()), caskFixtureSize)
	}
	for token, want := range map[string]string{"slack": "slack", "google-cloud-sdk": "gcloud-cli", "4k-video-downloaderplus": "4k-video-downloader+"} {
		if c, ok := idx.Lookup(token); !ok || c.Token != want {
			t.Errorf("Lookup(%q) = %v, %v; want %s", token, c, ok, want)
		}
	}
	if _, ok := idx.Lookup("nosuchcask"); ok {
		t.Error("found a cask that does not exist")
	}

	// A cask's own token wins over another's old token.
	clash := NewCaskIndex([]Cask{{Token: "b", OldTokens: []string{"a"}}, {Token: "a"}})
	if c, _ := clash.Lookup("a"); c.Token != "a" {
		t.Errorf("Lookup(a) = %s", c.Token)
	}
}

// listings serves the formula and cask fixtures at /formula and /cask.
func listings(t *testing.T) (*httptest.Server, *server, *server) {
	t.Helper()
	formulae, casks := &server{body: fixture(t)}, &server{body: caskFixture(t)}
	mux := http.NewServeMux()
	mux.Handle("/formula", formulae)
	mux.Handle("/cask", casks)
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, formulae, casks
}

func TestCasksAreCachedApartFromFormulae(t *testing.T) {
	ts, formulae, casks := listings(t)
	dir := t.TempDir()
	clk := &clock{}
	c := &Client{Key: testKey(), URL: ts.URL + "/formula", CaskURL: ts.URL + "/cask", CacheDir: dir, Now: clk.now}
	if fs, err := c.Formulae(context.Background()); err != nil || len(fs) != fixtureSize {
		t.Fatalf("got %d formulae, %v", len(fs), err)
	}
	if cs, err := casksOf(c); err != nil || len(cs) != caskFixtureSize {
		t.Fatalf("got %d casks, %v", len(cs), err)
	}
	for _, name := range []string{"cask.json", "cask.etag", "cask.index", "formula.json", "formula.etag", "formula.index"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Error(err)
		}
	}

	// While fresh, the cask index answers without the server or the body.
	if err := os.WriteFile(filepath.Join(dir, "cask.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk.offset = time.Minute
	if cs, err := casksOf(c); err != nil || len(cs) != caskFixtureSize {
		t.Fatalf("from the index: got %d casks, %v", len(cs), err)
	}
	if formulae.hits.Load() != 1 || casks.hits.Load() != 1 {
		t.Errorf("hits = %d formula, %d cask; want 1 each", formulae.hits.Load(), casks.hits.Load())
	}
}

func TestCasksStaleCacheWarnsAboutCasks(t *testing.T) {
	ts, _, _ := listings(t)
	dir := t.TempDir()
	if _, err := casksOf(&Client{Key: testKey(), CaskURL: ts.URL + "/cask", CacheDir: dir}); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	c := &Client{Key: testKey(), CaskURL: "http://127.0.0.1:1", CacheDir: dir, Now: afterFresh, Warn: func(m string) { warnings = append(warnings, m) }}
	if cs, err := casksOf(c); err != nil || len(cs) != caskFixtureSize {
		t.Fatalf("got %d casks, %v; want the stale cache", len(cs), err)
	}
	if len(warnings) != 1 || !strings.HasPrefix(warnings[0], "using the cached cask list") {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestCasksRejectAnUnsignedListing(t *testing.T) {
	ts := httptest.NewServer(&server{body: caskFixture(t), raw: true})
	defer ts.Close()
	dir := t.TempDir()
	_, err := casksOf(&Client{Key: testKey(), CaskURL: ts.URL, CacheDir: dir})
	if err == nil || !strings.Contains(err.Error(), "the cask listing") {
		t.Errorf("err = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("unsigned listing was cached: %v", entries)
	}
}

// casksOf loads the cask listing and returns the casks in it.
func casksOf(c *Client) ([]*Cask, error) {
	idx, err := c.CaskIndex(context.Background())
	if err != nil {
		return nil, err
	}
	return idx.Casks(), nil
}

// indexedCasks fetches the cask listing, then loads it again from the index.
func indexedCasks(t *testing.T) (*Client, *CaskIndex) {
	ts, _, _ := listings(t)
	clk := &clock{}
	c := &Client{Key: testKey(), CaskURL: ts.URL + "/cask", CacheDir: t.TempDir(), Now: clk.now}
	if _, err := c.CaskIndex(context.Background()); err != nil {
		t.Fatal(err)
	}
	clk.offset = time.Minute
	idx, err := c.CaskIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return c, idx
}

func TestCaskIndexMatchesAFreshParse(t *testing.T) {
	_, idx := indexedCasks(t)
	parsed, err := ParseCasks(caskFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	loaded := idx.Casks()
	if len(parsed) != len(loaded) {
		t.Fatalf("%d casks parsed, %d loaded", len(parsed), len(loaded))
	}
	if slack := loaded[slices.IndexFunc(loaded, func(c *Cask) bool { return c.Token == "slack" })]; slack.Artifacts != nil || slack.Variations != nil {
		t.Error("the index holds artifacts and variations, which Lookup reads from the listing")
	}
	for i := range parsed {
		p := &parsed[i]
		l, ok := idx.Lookup(p.Token)
		if !ok {
			t.Errorf("%s: not found", p.Token)
			continue
		}
		// %+v prints the pointer, not what it points to.
		if !reflect.DeepEqual(p.DependsOn.MacOS, l.DependsOn.MacOS) {
			t.Errorf("%s macos differs after the round trip: %+v, %+v", p.Token, p.DependsOn.MacOS, l.DependsOn.MacOS)
		}
		pc, lc := *p, *l
		pc.DependsOn.MacOS, lc.DependsOn.MacOS = nil, nil
		if a, b := fmt.Sprintf("%+v", pc), fmt.Sprintf("%+v", lc); a != b {
			t.Errorf("%s differs after the round trip\nparsed: %s\nloaded: %s", p.Token, a, b)
		}
	}
	slack, ok := idx.Lookup("slack")
	if !ok || len(slack.Artifacts) == 0 || len(slack.Variations) == 0 {
		t.Errorf("slack lost data: %+v", slack)
	}
}

func TestCaskLookupParsesTheListingWhenItMoved(t *testing.T) {
	c, idx := indexedCasks(t)
	file := filepath.Join(c.CacheDir, "cask.json")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, append([]byte("    "), data...), 0o644); err != nil {
		t.Fatal(err)
	}
	if slack, ok := idx.Lookup("slack"); !ok || len(slack.Artifacts) == 0 {
		t.Errorf("Lookup(slack) = %+v, %v; want it whole from a parse", slack, ok)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if _, ok := idx.Lookup("gcloud-cli"); ok {
		t.Error("Lookup found a cask it could not complete")
	}
}

func TestRefreshCasksReportsWhatChanged(t *testing.T) {
	var listing []map[string]any
	if err := json.Unmarshal(caskFixture(t), &listing); err != nil {
		t.Fatal(err)
	}
	removed := listing[0]["token"].(string)
	newer := listing[1:]
	added := map[string]any{}
	maps.Copy(added, listing[1])
	added["token"], added["full_token"], added["old_tokens"] = "new-cask", "new-cask", []string{}
	newer = append(newer, added)
	newBody, err := json.Marshal(newer)
	if err != nil {
		t.Fatal(err)
	}

	var body atomic.Pointer[[]byte]
	old := caskFixture(t)
	body.Store(&old)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(apitest.Listing(apitest.Shared(), *body.Load()))
	}))
	defer ts.Close()
	c := &Client{Key: testKey(), CaskURL: ts.URL, CacheDir: t.TempDir()}

	up, err := c.RefreshCasks(context.Background())
	if err != nil || !up.Changed || up.Total != caskFixtureSize || up.Added != nil {
		t.Fatalf("first refresh = %+v, %v", up, err)
	}
	if up, err = c.RefreshCasks(context.Background()); err != nil || up.Changed {
		t.Errorf("unchanged refresh = %+v, %v", up, err)
	}
	body.Store(&newBody)
	up, err = c.RefreshCasks(context.Background())
	if err != nil || !up.Changed || !slices.Equal(up.Added, []string{"new-cask"}) || !slices.Equal(up.Removed, []string{removed}) {
		t.Errorf("refresh = %+v, %v; want +new-cask and -%s", up, err, removed)
	}
}
