package api

import (
	"bytes"
	"context"
	"encoding/gob"
	"encoding/json"
	"fmt"
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
)

const fixtureSize = 19

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "formulae.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func parseFixture(t *testing.T) []Formula {
	t.Helper()
	fs, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	return fs
}

func TestParse(t *testing.T) {
	fs := parseFixture(t)
	if len(fs) != fixtureSize {
		t.Fatalf("got %d formulae", len(fs))
	}
	jq := fs[0]
	if jq.Name != "jq" || jq.Tap != "homebrew/core" || jq.Versions.Stable != "1.8.2" {
		t.Errorf("jq = %+v", jq)
	}
	if !reflect.DeepEqual(jq.Dependencies, []string{"oniguruma"}) {
		t.Errorf("Dependencies = %v", jq.Dependencies)
	}
	if jq.Bottle.Stable.Rebuild != 1 {
		t.Errorf("Rebuild = %d", jq.Bottle.Stable.Rebuild)
	}
	if !slices.Contains(jq.Bottle.Stable.Tags, "arm64_tahoe") || jq.Bottle.Stable.RootURL == "" {
		t.Errorf("bottle = %+v", jq.Bottle.Stable)
	}
	if !fs[1].KegOnly {
		t.Error("openssl@3 should be keg_only")
	}
	if len(fs[3].PostInstallSteps) == 0 {
		t.Error("python@3.13 should have post_install_steps")
	}
}

func TestParseInvalid(t *testing.T) {
	if _, err := Parse([]byte("[{")); err == nil {
		t.Error("expected error")
	}
}

func TestIndexLookup(t *testing.T) {
	idx := NewIndex(parseFixture(t))
	for _, name := range []string{"jq", "nodejs", "node@26", "openssl@3.6"} {
		if _, ok := idx.Lookup(name); !ok {
			t.Errorf("Lookup(%q) failed", name)
		}
	}
	if f, _ := idx.Lookup("npm"); f == nil || f.Name != "node" {
		t.Errorf("Lookup(npm) = %+v", f)
	}
	if _, ok := idx.Lookup("nope"); ok {
		t.Error("Lookup(nope) should fail")
	}
}

func TestBottleTags(t *testing.T) {
	tags := parseFixture(t)[0].BottleTags()
	for _, want := range []string{"arm64_tahoe", "arm64_linux", "x86_64_linux", "sonoma"} {
		if !slices.Contains(tags, want) {
			t.Errorf("BottleTags missing %q: %v", want, tags)
		}
	}
	if !slices.IsSorted(tags) {
		t.Errorf("tags not sorted: %v", tags)
	}
	if got := (&Formula{}).BottleTags(); len(got) != 0 {
		t.Errorf("no bottle should give no tags, got %v", got)
	}
}

type server struct {
	hits        atomic.Int32
	conditional atomic.Int32
	status      int
	body        []byte
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.hits.Add(1)
	if s.status != 0 {
		http.Error(w, "boom", s.status)
		return
	}
	if r.Header.Get("If-None-Match") == `"v1"` {
		s.conditional.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", `"v1"`)
	_, _ = w.Write(s.body)
}

// afterFresh is a clock far enough ahead that a cached listing needs revalidating.
func afterFresh() time.Time { return time.Now().Add(time.Hour) }

func TestFormulaeCachesWithETag(t *testing.T) {
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()}

	first, err := c.Formulae(context.Background())
	if err != nil || len(first) != fixtureSize {
		t.Fatalf("first fetch = %d formulae, %v", len(first), err)
	}
	c.Now = afterFresh
	second, err := c.Formulae(context.Background())
	if err != nil || len(second) != fixtureSize {
		t.Fatalf("second fetch = %d formulae, %v", len(second), err)
	}
	if srv.hits.Load() != 2 || srv.conditional.Load() != 1 {
		t.Errorf("hits = %d, conditional = %d; want 2 and 1", srv.hits.Load(), srv.conditional.Load())
	}
}

func TestFormulaeIgnoresETagWithoutBody(t *testing.T) {
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	c := &Client{URL: ts.URL, CacheDir: dir}
	if _, err := c.Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "formula.json")); err != nil {
		t.Fatal(err)
	}
	fs, err := c.Formulae(context.Background())
	if err != nil || len(fs) != fixtureSize {
		t.Fatalf("fetch with etag but no body = %d formulae, %v", len(fs), err)
	}
	if srv.conditional.Load() != 0 {
		t.Error("sent If-None-Match without a cached body")
	}
}

func TestFormulaeServerError(t *testing.T) {
	ts := httptest.NewServer(&server{status: http.StatusInternalServerError})
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()}
	if _, err := c.Formulae(context.Background()); err == nil {
		t.Error("expected error")
	}
}

func TestFormulaeStaleCacheWhenFetchFails(t *testing.T) {
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	dir := t.TempDir()
	if _, err := (&Client{URL: ts.URL, CacheDir: dir}).Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	down := ts.URL
	ts.Close()
	failing := httptest.NewServer(&server{status: http.StatusInternalServerError})
	defer failing.Close()

	for name, url := range map[string]string{"unreachable": down, "server error": failing.URL} {
		t.Run(name, func(t *testing.T) {
			var warnings []string
			c := &Client{URL: url, CacheDir: dir, Now: afterFresh, Warn: func(msg string) { warnings = append(warnings, msg) }}
			fs, err := c.Formulae(context.Background())
			if err != nil || len(fs) != fixtureSize {
				t.Fatalf("got %d formulae, %v; want the stale cache", len(fs), err)
			}
			if len(warnings) != 1 {
				t.Errorf("warnings = %q, want one", warnings)
			}
		})
	}
}

func TestFormulaeNoWarningWhenFresh(t *testing.T) {
	ts := httptest.NewServer(&server{body: fixture(t)})
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir(), Warn: func(msg string) { t.Errorf("unexpected warning %q", msg) }}
	for range 2 {
		if _, err := c.Formulae(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFormulaeUnreachableWithoutCache(t *testing.T) {
	ts := httptest.NewServer(&server{})
	url := ts.URL
	ts.Close()
	c := &Client{URL: url, CacheDir: t.TempDir(), Warn: func(msg string) { t.Errorf("unexpected warning %q", msg) }}
	if _, err := c.Formulae(context.Background()); err == nil {
		t.Error("expected error")
	}
}

func TestFormulaeBadBodyNotCached(t *testing.T) {
	srv := &server{body: []byte("[{")}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	c := &Client{URL: ts.URL, CacheDir: dir}
	if _, err := c.Formulae(context.Background()); err == nil {
		t.Fatal("expected parse error")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("bad body was cached: %v", entries)
	}
}

func forTag(t *testing.T, f Formula, tag string) Formula {
	t.Helper()
	got, err := f.ForTag(tag)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func byName(t *testing.T, name string) Formula {
	t.Helper()
	f, ok := NewIndex(parseFixture(t)).Lookup(name)
	if !ok {
		t.Fatalf("%s not in fixture", name)
	}
	return *f
}

func TestUsesFromMacOS(t *testing.T) {
	node := byName(t, "node")
	if len(node.UsesFromMacOS) != 1 || node.UsesFromMacOS[0].Name != "python" || !node.UsesFromMacOS[0].Runtime() {
		t.Errorf("node uses = %+v", node.UsesFromMacOS)
	}
	xz := byName(t, "zsh-lovers").UsesFromMacOS
	if len(xz) != 1 || xz[0].Name != "xz" || !reflect.DeepEqual(xz[0].Tags, []string{"build"}) || xz[0].Runtime() {
		t.Errorf("zsh-lovers uses = %+v", xz)
	}
}

func TestUsesFromMacOSShapes(t *testing.T) {
	var got []UsesFromMacOS
	in := `["a", {"b": "build"}, {"c": ["build", "test"]}, {"d": "test"}, {"e": "no_linkage"}]`
	if err := json.Unmarshal([]byte(in), &got); err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name    string
		runtime bool
	}{{"a", true}, {"b", false}, {"c", false}, {"d", false}, {"e", true}}
	if len(got) != len(want) {
		t.Fatalf("got %d entries", len(got))
	}
	for i, w := range want {
		if got[i].Name != w.name || got[i].Runtime() != w.runtime {
			t.Errorf("entry %d = %+v, want %s runtime=%v", i, got[i], w.name, w.runtime)
		}
	}
	if err := json.Unmarshal([]byte(`[{"a": "build", "b": "test"}]`), &got); err == nil {
		t.Error("an object with two names should be rejected")
	}
	if err := json.Unmarshal([]byte(`[3]`), &got); err == nil {
		t.Error("a number should be rejected")
	}
}

func TestVersionScheme(t *testing.T) {
	if got := byName(t, "utf8cpp").VersionScheme; got == 0 {
		t.Errorf("utf8cpp VersionScheme = %d", got)
	}
	if got := byName(t, "jq").VersionScheme; got != 0 {
		t.Errorf("jq VersionScheme = %d", got)
	}
}

func TestForTag(t *testing.T) {
	crystal := byName(t, "crystal")
	base := slices.Clone(crystal.Dependencies)

	linux := forTag(t, crystal, "x86_64_linux")
	if slices.Contains(base, "libffi") || len(linux.Dependencies) != len(base)+1 || !slices.Contains(linux.Dependencies, "libffi") {
		t.Errorf("x86_64_linux dependencies = %v, want base %v plus libffi", linux.Dependencies, base)
	}
	if !reflect.DeepEqual(crystal.Dependencies, base) {
		t.Errorf("ForTag changed the original: %v", crystal.Dependencies)
	}
	if got := forTag(t, crystal, "arm64_tahoe").Dependencies; !reflect.DeepEqual(got, base) {
		t.Errorf("arm64_tahoe dependencies = %v, want the base list", got)
	}
	if got := forTag(t, crystal, "nonsense").Dependencies; !reflect.DeepEqual(got, base) {
		t.Errorf("unknown tag dependencies = %v", got)
	}
	if got := forTag(t, crystal, "").Dependencies; !reflect.DeepEqual(got, base) {
		t.Errorf("empty tag dependencies = %v", got)
	}
}

func TestForTagCanEmptyDependencies(t *testing.T) {
	semtag := byName(t, "semtag")
	if len(semtag.Dependencies) == 0 {
		t.Fatal("fixture should have a base dependency")
	}
	if got := forTag(t, semtag, "x86_64_linux").Dependencies; len(got) != 0 {
		t.Errorf("override with [] should empty the list, got %v", got)
	}
}

func TestForTagKegOnly(t *testing.T) {
	f := Formula{Variations: map[string]json.RawMessage{"x": json.RawMessage(`{"keg_only": true}`)}}
	if f.KegOnly || !forTag(t, f, "x").KegOnly {
		t.Error("keg_only override not applied")
	}
}

func TestForTagMalformedVariation(t *testing.T) {
	f := Formula{Name: "x", Variations: map[string]json.RawMessage{"t": json.RawMessage(`{"dependencies": "oops"}`)}}
	if _, err := f.ForTag("t"); err == nil {
		t.Error("expected an error for a malformed variation")
	}
}

func TestUsesFromMacOSSince(t *testing.T) {
	dzr := byName(t, "dzr").UsesFromMacOS
	if len(dzr) != 1 || dzr[0].Name != "jq" || dzr[0].Since != "sequoia" {
		t.Errorf("dzr uses = %+v", dzr)
	}
	if got := byName(t, "node").UsesFromMacOS[0].Since; got != "" {
		t.Errorf("node python Since = %q, want none", got)
	}
}

func TestIndexFormulae(t *testing.T) {
	idx := NewIndex(parseFixture(t))
	all := idx.Formulae()
	if len(all) != fixtureSize || all[0].Name != "jq" || all[1].Name != "openssl@3" {
		t.Errorf("Formulae() = %d entries starting %q, %q", len(all), all[0].Name, all[1].Name)
	}
}

func TestInfoFields(t *testing.T) {
	git := byName(t, "git")
	if git.Homepage != "https://git-scm.com" || git.RubySourcePath != "Formula/g/git.rb" {
		t.Errorf("homepage = %q, path = %q", git.Homepage, git.RubySourcePath)
	}
	if git.Versions.Head != "HEAD" || byName(t, "jq").Versions.Head != "HEAD" || byName(t, "xz").Versions.Head != "" {
		t.Error("head versions wrong")
	}
	if !strings.HasPrefix(git.Caveats, "The Tcl/Tk GUIs") || git.License == "" {
		t.Errorf("caveats = %q, license = %q", git.Caveats, git.License)
	}
	if !slices.Equal(git.BuildDependencies, []string{"gettext", "pkgconf"}) {
		t.Errorf("build deps = %v", git.BuildDependencies)
	}
	ossl := byName(t, "openssl@3")
	if ossl.KegOnlyReason.Reason != ":versioned_formula" || !slices.Contains(ossl.VersionedFormulae, "openssl@4") {
		t.Errorf("keg-only reason = %+v, versioned = %v", ossl.KegOnlyReason, ossl.VersionedFormulae)
	}
	if got := byName(t, "readline").KegOnlyReason.Explanation; got != "macOS provides BSD libedit" {
		t.Errorf("explanation = %q", got)
	}
	if jq := byName(t, "jq"); jq.Deprecated || jq.Disabled || len(jq.OldNames) != 0 {
		t.Errorf("jq flags = %v %v %v", jq.Deprecated, jq.Disabled, jq.OldNames)
	}
}

func TestRaw(t *testing.T) {
	ts := httptest.NewServer(&server{body: fixture(t)})
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()}
	if _, err := c.Raw("jq"); err == nil {
		t.Error("expected an error before anything is cached")
	}
	if _, err := c.Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	raw, err := c.Raw("jq")
	if err != nil {
		t.Fatal(err)
	}
	var got struct{ Name, Desc string }
	if err := json.Unmarshal(raw, &got); err != nil || got.Name != "jq" || got.Desc == "" {
		t.Errorf("Raw(jq) = %s, %v", raw, err)
	}
	if !strings.Contains(string(raw), `"homepage"`) || !strings.Contains(string(raw), `"sha256"`) {
		t.Error("raw entry is missing fields, such as the bottle checksums a formula does not keep")
	}
	if _, err := c.Raw("nosuch"); err == nil {
		t.Error("expected an error for an unknown formula")
	}
}

func TestConflictReasonsNullBecomesEmpty(t *testing.T) {
	fs, err := Parse([]byte(`[{"name":"x","conflicts_with":["a","b"],"conflicts_with_reasons":["both install x",null]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if got := fs[0].ConflictsWithReasons; !reflect.DeepEqual(got, []string{"both install x", ""}) {
		t.Errorf("reasons = %q", got)
	}
}

// clock is a Now function that tests can move.
type clock struct{ offset time.Duration }

func (c *clock) now() time.Time { return time.Now().Add(c.offset) }

func TestFormulaeSkipsTheServerWhileFresh(t *testing.T) {
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	clk := &clock{}
	c := &Client{URL: ts.URL, CacheDir: t.TempDir(), Now: clk.now}
	get := func(want int32) {
		t.Helper()
		fs, err := c.Formulae(context.Background())
		if err != nil || len(fs) != fixtureSize {
			t.Fatalf("got %d formulae, %v", len(fs), err)
		}
		if srv.hits.Load() != want {
			t.Fatalf("hits = %d, want %d", srv.hits.Load(), want)
		}
	}
	get(1)
	clk.offset = 5 * time.Minute
	get(1) // still fresh
	clk.offset = 11 * time.Minute
	get(2) // revalidated, the server says not modified
	if srv.conditional.Load() != 1 {
		t.Errorf("conditional = %d, want 1", srv.conditional.Load())
	}
	clk.offset = 15 * time.Minute
	get(2) // the revalidation made it fresh again
	clk.offset = 22 * time.Minute
	get(3)
}

func TestFormulaeLoadsTheIndexNotTheBody(t *testing.T) {
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	clk := &clock{}
	c := &Client{URL: ts.URL, CacheDir: dir, Now: clk.now}
	if _, err := c.Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "formula.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, offset := range []time.Duration{time.Minute, time.Hour} { // fresh, then revalidated with a 304
		clk.offset = offset
		fs, err := c.Formulae(context.Background())
		if err != nil || len(fs) != fixtureSize {
			t.Fatalf("offset %v: got %d formulae, %v", offset, len(fs), err)
		}
	}
}

func writeIndex(t *testing.T, dir string, idx listingIndex) {
	t.Helper()
	var b bytes.Buffer
	if err := gob.NewEncoder(&b).Encode(idx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "formula.index"), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFormulaeRebuildsAnUnusableIndex(t *testing.T) {
	tests := map[string]func(t *testing.T, dir string){
		"missing": func(t *testing.T, dir string) {
			if err := os.Remove(filepath.Join(dir, "formula.index")); err != nil {
				t.Fatal(err)
			}
		},
		"corrupt": func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "formula.index"), []byte("garbage"), 0o644); err != nil {
				t.Fatal(err)
			}
		},
		// An index that is well formed but not for this listing must not be used. It
		// holds no formulae, so using it would be noticed.
		"for another listing": func(t *testing.T, dir string) {
			writeIndex(t, dir, listingIndex{Fingerprint: indexFingerprint, ETag: `"v0"`})
		},
		"from another version of the formula type": func(t *testing.T, dir string) {
			writeIndex(t, dir, listingIndex{Fingerprint: "old", ETag: `"v1"`})
		},
	}
	for name, damage := range tests {
		t.Run(name, func(t *testing.T) {
			ts := httptest.NewServer(&server{body: fixture(t)})
			defer ts.Close()
			dir := t.TempDir()
			clk := &clock{}
			c := &Client{URL: ts.URL, CacheDir: dir, Now: clk.now}
			if _, err := c.Formulae(context.Background()); err != nil {
				t.Fatal(err)
			}
			damage(t, dir)
			clk.offset = time.Hour
			if fs, err := c.Formulae(context.Background()); err != nil || len(fs) != fixtureSize {
				t.Fatalf("got %d formulae, %v", len(fs), err)
			}
			// The rebuilt index is good: a fresh call must not need the body.
			if err := os.WriteFile(filepath.Join(dir, "formula.json"), []byte("not json"), 0o644); err != nil {
				t.Fatal(err)
			}
			clk.offset = time.Hour + time.Minute
			if fs, err := c.Formulae(context.Background()); err != nil || len(fs) != fixtureSize {
				t.Fatalf("after rebuild: got %d formulae, %v", len(fs), err)
			}
		})
	}
}

func TestFormulaeOfflineUsesTheIndex(t *testing.T) {
	ts := httptest.NewServer(&server{body: fixture(t)})
	dir := t.TempDir()
	clk := &clock{}
	if _, err := (&Client{URL: ts.URL, CacheDir: dir, Now: clk.now}).Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	down := ts.URL
	ts.Close()
	if err := os.WriteFile(filepath.Join(dir, "formula.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	clk.offset = time.Hour
	var warnings []string
	c := &Client{URL: down, CacheDir: dir, Now: clk.now, Warn: func(m string) { warnings = append(warnings, m) }}
	fs, err := c.Formulae(context.Background())
	if err != nil || len(fs) != fixtureSize || len(warnings) != 1 {
		t.Errorf("got %d formulae, %v, warnings %q", len(fs), err, warnings)
	}
}

func TestFormulaeNeedsTheBodyToStaySkippable(t *testing.T) {
	// The index alone must not satisfy a fresh call: Raw needs the body.
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	dir := t.TempDir()
	c := &Client{URL: ts.URL, CacheDir: dir}
	if _, err := c.Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "formula.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	if srv.hits.Load() != 2 {
		t.Errorf("hits = %d, want a refetch because the body is gone", srv.hits.Load())
	}
	if _, err := c.Raw("jq"); err != nil {
		t.Errorf("Raw after the refetch: %v", err)
	}
}

func TestFingerprint(t *testing.T) {
	type a struct {
		X int
		Y []string
	}
	type b struct {
		X int
		Y []string
		Z bool
	}
	type c struct {
		X int
		Y []int
	}
	fa, fb, fc := fingerprint(reflect.TypeOf(a{})), fingerprint(reflect.TypeOf(b{})), fingerprint(reflect.TypeOf(c{}))
	if fa != fingerprint(reflect.TypeOf(a{})) {
		t.Error("fingerprint is not stable")
	}
	if fa == fb || fa == fc || fb == fc {
		t.Errorf("added fields or changed types must change the fingerprint: %s %s %s", fa, fb, fc)
	}
}

func TestIndexMatchesAFreshParse(t *testing.T) {
	ts := httptest.NewServer(&server{body: fixture(t)})
	defer ts.Close()
	clk := &clock{}
	c := &Client{URL: ts.URL, CacheDir: t.TempDir(), Now: clk.now}
	parsed, err := c.Formulae(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	clk.offset = time.Minute
	loaded, err := c.Formulae(context.Background()) // from the index
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed) != len(loaded) {
		t.Fatalf("%d formulae parsed, %d loaded", len(parsed), len(loaded))
	}
	for i := range parsed {
		// %+v prints nil and empty slices alike, which is the difference gob loses.
		if a, b := fmt.Sprintf("%+v", parsed[i]), fmt.Sprintf("%+v", loaded[i]); a != b {
			t.Errorf("%s differs after the round trip\nparsed: %s\nloaded: %s", parsed[i].Name, a, b)
		}
	}
	// What the commands rely on survives as is.
	idx := NewIndex(loaded)
	if f, ok := idx.Lookup("python@3.13"); !ok || len(f.UsesFromMacOS) == 0 || len(f.Variations) == 0 || len(f.BottleTags()) == 0 {
		t.Errorf("python@3.13 lost data: %+v", f)
	}
}

func TestBottles(t *testing.T) {
	ts := httptest.NewServer(&server{body: fixture(t)})
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()}
	if _, err := c.Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := c.Bottles(map[string]string{"jq": "arm64_tahoe", "xz": "arm64_tahoe"})
	if err != nil {
		t.Fatal(err)
	}
	jq := got["jq"]
	if jq.Cellar != ":any" || len(jq.SHA256) != 64 || !strings.HasPrefix(jq.URL, "https://") || !strings.HasSuffix(jq.URL, "sha256:"+jq.SHA256) {
		t.Errorf("jq = %+v", jq)
	}
	if len(got) != 2 || got["xz"].SHA256 == "" {
		t.Errorf("got %d bottles: %+v", len(got), got)
	}
	if got, err := c.Bottles(nil); err != nil || len(got) != 0 {
		t.Errorf("Bottles(nil) = %v, %v", got, err)
	}

	for name, want := range map[string]map[string]string{
		"an unknown formula": {"nosuchformula": "arm64_tahoe"},
		"a tag it lacks":     {"jq": "arm64_windows"},
	} {
		if _, err := c.Bottles(want); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestRefreshAsksTheServerEvenWhenTheCacheIsFresh(t *testing.T) {
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()}
	if _, err := c.Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	up, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if srv.hits.Load() != 2 || srv.conditional.Load() != 1 {
		t.Errorf("hits = %d, conditional = %d; want 2 and 1", srv.hits.Load(), srv.conditional.Load())
	}
	if up.Changed || up.Total != fixtureSize {
		t.Errorf("update = %+v, want unchanged with %d formulae", up, fixtureSize)
	}
}

func TestRefreshFirstFetch(t *testing.T) {
	ts := httptest.NewServer(&server{body: fixture(t)})
	defer ts.Close()
	up, err := (&Client{URL: ts.URL, CacheDir: t.TempDir()}).Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !up.Changed || up.Total != fixtureSize || len(up.Added) != 0 || len(up.Removed) != 0 {
		t.Errorf("update = %+v: a first fetch is a change without a list of what is new", up)
	}
}

func TestRefreshReportsWhatChanged(t *testing.T) {
	var listing []map[string]any
	if err := json.Unmarshal(fixture(t), &listing); err != nil {
		t.Fatal(err)
	}
	removed := listing[0]["name"].(string)
	newer := listing[1:]
	for _, name := range []string{"new-b", "new-a"} { // out of order, as a listing may be
		added := map[string]any{}
		for k, v := range listing[1] {
			added[k] = v
		}
		added["name"], added["full_name"], added["aliases"], added["oldnames"] = name, name, []string{}, []string{}
		newer = append(newer, added)
	}
	newer2, err := json.Marshal(newer)
	if err != nil {
		t.Fatal(err)
	}
	var current atomic.Pointer[[]byte]
	old := fixture(t)
	current.Store(&old)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(*current.Load()) }))
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()}
	if _, err := c.Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	current.Store(&newer2)
	up, err := c.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !up.Changed || up.Total != fixtureSize+1 || !slices.Equal(up.Added, []string{"new-a", "new-b"}) || !slices.Equal(up.Removed, []string{removed}) {
		t.Errorf("update = %+v, want +new-a, +new-b and -%s", up, removed)
	}
	if got, err := c.Formulae(context.Background()); err != nil || !slices.ContainsFunc(got, func(f Formula) bool { return f.Name == "new-a" }) {
		t.Errorf("the new listing was not cached: %v", err)
	}
}

func TestRefreshFailsInsteadOfUsingTheCache(t *testing.T) {
	ts := httptest.NewServer(&server{body: fixture(t)})
	dir := t.TempDir()
	if _, err := (&Client{URL: ts.URL, CacheDir: dir}).Formulae(context.Background()); err != nil {
		t.Fatal(err)
	}
	failing := httptest.NewServer(&server{status: http.StatusInternalServerError})
	defer failing.Close()
	for name, url := range map[string]string{"unreachable": ts.URL, "server error": failing.URL} {
		if name == "unreachable" {
			ts.Close()
		}
		c := &Client{URL: url, CacheDir: dir, Warn: func(msg string) { t.Errorf("unexpected warning %q", msg) }}
		if _, err := c.Refresh(context.Background()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if got, err := (&Client{URL: "http://127.0.0.1:1", CacheDir: dir}).Formulae(context.Background()); err != nil || len(got) != fixtureSize {
		t.Errorf("the cache was damaged: %d formulae, %v", len(got), err)
	}
}
