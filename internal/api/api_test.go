package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
)

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
	if len(fs) != 4 {
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
	w.Write(s.body)
}

func TestFormulaeCachesWithETag(t *testing.T) {
	srv := &server{body: fixture(t)}
	ts := httptest.NewServer(srv)
	defer ts.Close()
	c := &Client{URL: ts.URL, CacheDir: t.TempDir()}

	first, err := c.Formulae(context.Background())
	if err != nil || len(first) != 4 {
		t.Fatalf("first fetch = %d formulae, %v", len(first), err)
	}
	second, err := c.Formulae(context.Background())
	if err != nil || len(second) != 4 {
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
	if err != nil || len(fs) != 4 {
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
