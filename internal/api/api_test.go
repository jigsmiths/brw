package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
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
	f := jq.Bottle.Stable.Files["arm64_tahoe"]
	if f.Cellar != ":any" || len(f.SHA256) != 64 || f.URL == "" {
		t.Errorf("arm64_tahoe file = %+v", f)
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

func TestFormulaeCachesWithETag(t *testing.T) {
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()}

	first, err := c.Formulae(context.Background())
	if err != nil || len(first) != fixtureSize {
		t.Fatalf("first fetch = %d formulae, %v", len(first), err)
	}
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
			c := &Client{URL: url, CacheDir: dir, Warn: func(msg string) { warnings = append(warnings, msg) }}
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
