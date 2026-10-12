package bottle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
)

const tabJSON = `{"homebrew_version":"6.0.2","changed_files":["lib/x.pc"],"built_on":{"os":"Macintosh"}}`

// registry plays ghcr.io and the CDN it redirects blobs to.
type registry struct {
	t        *testing.T
	body     []byte
	sum      string
	registry *httptest.Server
	cdn      *httptest.Server

	mu          sync.Mutex
	registryHit []string // "METHOD path auth"
	cdnAuth     []string
	manifests   atomic.Int32
	blobs       atomic.Int32
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
	delay       time.Duration
	failBlob    int
}

func newRegistry(t *testing.T, body string) *registry {
	t.Helper()
	sum := sha256.Sum256([]byte(body))
	r := &registry{t: t, body: []byte(body), sum: hex.EncodeToString(sum[:])}
	r.cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.cdnAuth = append(r.cdnAuth, req.Header.Get("Authorization"))
		r.mu.Unlock()
		cur := r.inFlight.Add(1)
		defer r.inFlight.Add(-1)
		for {
			old := r.maxInFlight.Load()
			if cur <= old || r.maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		time.Sleep(r.delay)
		_, _ = w.Write(r.body)
	}))
	r.registry = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.registryHit = append(r.registryHit, req.Method+" "+req.URL.Path+" "+req.Header.Get("Authorization"))
		r.mu.Unlock()
		switch {
		case !strings.HasPrefix(req.URL.Path, "/v2/homebrew/core/"):
			http.NotFound(w, req)
		case strings.Contains(req.URL.Path, "/manifests/"):
			r.manifests.Add(1)
			if req.Header.Get("Accept") != "application/vnd.oci.image.index.v1+json" {
				http.Error(w, "wrong accept", http.StatusNotAcceptable)
				return
			}
			index := map[string]any{"manifests": []map[string]any{
				{"annotations": map[string]string{"sh.brew.bottle.digest": strings.Repeat("0", 64), "sh.brew.tab": `{"wrong":true}`}},
				{"annotations": map[string]string{"sh.brew.bottle.digest": r.sum, "sh.brew.tab": tabJSON}},
			}}
			_ = json.NewEncoder(w).Encode(index)
		case strings.Contains(req.URL.Path, "/blobs/"):
			n := int(r.blobs.Add(1))
			if r.failBlob != 0 && n == r.failBlob {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			http.Redirect(w, req, r.cdn.URL+"/blob", http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(func() { r.registry.Close(); r.cdn.Close() })
	return r
}

func (r *registry) request(name string) Request {
	return Request{
		Name: name, PkgVersion: "1.8.2", Rebuild: 1, Tag: "arm64_tahoe",
		File: api.BottleFile{Cellar: ":any", SHA256: r.sum,
			URL: r.registry.URL + "/v2/homebrew/core/" + name + "/blobs/sha256:" + r.sum},
	}
}

func fetcher(t *testing.T) *Fetcher { return &Fetcher{Dir: filepath.Join(t.TempDir(), "downloads")} }

func TestFetch(t *testing.T) {
	r := newRegistry(t, "bottle bytes")
	res, err := fetcher(t).Fetch(context.Background(), r.request("jq"))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(res.Path); err != nil || string(got) != "bottle bytes" {
		t.Errorf("file = %q, %v", got, err)
	}
	if string(res.Tab) != tabJSON {
		t.Errorf("tab = %s, want the matching manifest's", res.Tab)
	}
	if want := "jq--1.8.2.arm64_tahoe.bottle.1.tar.gz"; filepath.Base(res.Path) != want {
		t.Errorf("name = %s, want %s", filepath.Base(res.Path), want)
	}
	// The manifest tag is version-rebuild, and the registry sees the static token.
	if len(r.registryHit) != 2 || r.registryHit[0] != "GET /v2/homebrew/core/jq/manifests/1.8.2-1 Bearer QQ==" ||
		!strings.HasPrefix(r.registryHit[1], "GET /v2/homebrew/core/jq/blobs/sha256:") || !strings.HasSuffix(r.registryHit[1], " Bearer QQ==") {
		t.Errorf("registry requests = %q", r.registryHit)
	}
	if len(r.cdnAuth) != 1 || r.cdnAuth[0] != "" {
		t.Errorf("the CDN got authorization %q; it must not receive the token", r.cdnAuth)
	}
}

func TestFetchWithoutRebuildUsesTheBareVersion(t *testing.T) {
	r := newRegistry(t, "x")
	req := r.request("jq")
	req.Rebuild = 0
	res, err := fetcher(t).Fetch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(r.registryHit[0], "/manifests/1.8.2 Bearer QQ==") || filepath.Base(res.Path) != "jq--1.8.2.arm64_tahoe.bottle.tar.gz" {
		t.Errorf("hit %q, file %s", r.registryHit[0], res.Path)
	}
}

func TestFetchUsesTheCache(t *testing.T) {
	r := newRegistry(t, "bottle bytes")
	f := fetcher(t)
	first, err := f.Fetch(context.Background(), r.request("jq"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.Fetch(context.Background(), r.request("jq"))
	if err != nil {
		t.Fatal(err)
	}
	if r.manifests.Load() != 1 || r.blobs.Load() != 1 {
		t.Errorf("manifests %d, blobs %d; a cached bottle needs no requests", r.manifests.Load(), r.blobs.Load())
	}
	if second.Path != first.Path || string(second.Tab) != tabJSON {
		t.Errorf("second = %+v", second)
	}
}

func TestFetchReplacesACorruptCacheEntry(t *testing.T) {
	r := newRegistry(t, "bottle bytes")
	f := fetcher(t)
	res, err := f.Fetch(context.Background(), r.request("jq"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(res.Path, []byte("truncated"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = f.Fetch(context.Background(), r.request("jq"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(res.Path); string(got) != "bottle bytes" {
		t.Errorf("file = %q, want a fresh download", got)
	}
	if r.blobs.Load() != 2 {
		t.Errorf("blobs = %d, want a second download", r.blobs.Load())
	}
}

func TestFetchRejectsAWrongChecksum(t *testing.T) {
	r := newRegistry(t, "bottle bytes")
	f := fetcher(t)
	req := r.request("jq")
	req.File.SHA256 = strings.Repeat("a", 64) // the registry will not have a manifest for it either
	if _, err := f.Fetch(context.Background(), req); err == nil {
		t.Fatal("expected an error")
	}

	// A manifest that matches but a body that does not.
	r2 := newRegistry(t, "bottle bytes")
	r2.body = []byte("tampered bytes")
	if _, err := f.Fetch(context.Background(), r2.request("jq")); err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Errorf("error = %v, want a checksum error", err)
	}
	entries, _ := os.ReadDir(f.Dir)
	if len(entries) != 0 {
		t.Errorf("cache holds %v after failed downloads", entries)
	}
}

func TestFetchHTTPErrors(t *testing.T) {
	r := newRegistry(t, "x")
	r.failBlob = 1
	_, err := fetcher(t).Fetch(context.Background(), r.request("jq"))
	if err == nil || !strings.Contains(err.Error(), "500") {
		t.Errorf("blob error = %v", err)
	}
	missing := r.request("jq")
	missing.File.URL = r.registry.URL + "/nothing/here/blobs/sha256:" + r.sum
	_, err = fetcher(t).Fetch(context.Background(), missing)
	if err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("manifest error = %v", err)
	}
}

func TestFetchNeedsABlobURL(t *testing.T) {
	r := newRegistry(t, "x")
	req := r.request("jq")
	req.File.URL = "https://example.com/not-a-blob"
	if _, err := fetcher(t).Fetch(context.Background(), req); err == nil {
		t.Error("expected an error for a URL without /blobs/")
	}
}

func TestFetchAll(t *testing.T) {
	r := newRegistry(t, "x")
	r.delay = 30 * time.Millisecond
	f := fetcher(t)
	f.Workers = 3
	var reqs []Request
	for i := range 9 {
		reqs = append(reqs, r.request(fmt.Sprintf("pkg%d", i)))
	}
	results, err := f.FetchAll(context.Background(), reqs)
	if err != nil {
		t.Fatal(err)
	}
	for i, res := range results {
		if want := fmt.Sprintf("pkg%d--", i); !strings.HasPrefix(filepath.Base(res.Path), want) {
			t.Errorf("result %d = %s, want it in request order", i, res.Path)
		}
	}
	if m := r.maxInFlight.Load(); m < 2 || m > 3 {
		t.Errorf("up to %d downloads at once, want 2 or 3 with 3 workers", m)
	}
}

func TestFetchAllReportsEveryFailure(t *testing.T) {
	r := newRegistry(t, "x")
	good, bad := r.request("good"), r.request("bad")
	bad.File.URL = r.registry.URL + "/nothing/blobs/sha256:" + r.sum
	_, err := fetcher(t).FetchAll(context.Background(), []Request{good, bad})
	if err == nil || !strings.Contains(err.Error(), "bad") {
		t.Errorf("error = %v, want one that names the failing formula", err)
	}
	if errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "bad") {
		t.Errorf("the cancellation hid the real error: %v", err)
	}
}
