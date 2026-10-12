// Package installtest serves bottles like ghcr.io, for tests of installs.
package installtest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
)

var modTime = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// File is a file of a bottle, relative to name/version. A name ending in a slash
// is a directory. A zero Mode means 0644.
type File struct {
	Name, Body string
	Mode       int64
}

// Bottle describes a bottle and the tab in its manifest.
type Bottle struct {
	Name, Version string
	Files         []File
	Changed       []string // the tab's changed_files
	Linkage       []string // and linkage_files
	Deps          []string // and the formulae in runtime_dependencies
}

// Registry serves manifests and blobs. A blob is redirected to a second server,
// as ghcr.io does.
type Registry struct {
	t         testing.TB
	URL       string
	blobs     map[string][]byte // by digest
	tabs      map[string]string // by manifest path
	digests   map[string]string
	tampered  map[string]bool
	manifests atomic.Int32
}

func NewRegistry(t testing.TB) *Registry {
	t.Helper()
	r := &Registry{t: t, blobs: map[string][]byte{}, tabs: map[string]string{}, digests: map[string]string{}, tampered: map[string]bool{}}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		digest := strings.TrimPrefix(req.URL.Path, "/blob/")
		body := r.blobs[digest]
		if r.tampered[digest] {
			body = append([]byte("x"), body...)
		}
		_, _ = w.Write(body)
	}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v2/homebrew/core/")
		switch {
		case strings.Contains(path, "/manifests/"):
			r.manifests.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []map[string]any{
				{"annotations": map[string]string{"sh.brew.bottle.digest": r.digests[path], "sh.brew.tab": r.tabs[path]}},
			}})
		case strings.Contains(path, "/blobs/sha256:"):
			_, digest, _ := strings.Cut(path, "sha256:")
			http.Redirect(w, req, cdn.URL+"/blob/"+digest, http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(func() { server.Close(); cdn.Close() })
	r.URL = server.URL
	return r
}

// Tamper makes the registry serve other bytes than the bottle's checksum says.
func (r *Registry) Tamper(file api.BottleFile) { r.tampered[file.SHA256] = true }

// ManifestRequests counts the manifests served so far.
func (r *Registry) ManifestRequests() int { return int(r.manifests.Load()) }

// Add stores a bottle and returns its file as the API would describe it.
func (r *Registry) Add(b Bottle, cellar string) api.BottleFile {
	r.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	keg := b.Name + "/" + b.Version
	write := func(name string, typ byte, body string, mode int64) {
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Mode: mode, Size: int64(len(body)), ModTime: modTime}); err != nil {
			r.t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			r.t.Fatal(err)
		}
	}
	write(keg, tar.TypeDir, "", 0o755)
	for _, f := range b.Files {
		switch {
		case strings.HasSuffix(f.Name, "/"):
			write(keg+"/"+f.Name, tar.TypeDir, "", 0o755)
		case f.Mode == 0:
			write(keg+"/"+f.Name, tar.TypeReg, f.Body, 0o644)
		default:
			write(keg+"/"+f.Name, tar.TypeReg, f.Body, f.Mode)
		}
	}
	if err := tw.Close(); err != nil {
		r.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		r.t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	digest := hex.EncodeToString(sum[:])
	r.blobs[digest] = buf.Bytes()

	var runtime []map[string]any
	for _, d := range b.Deps {
		runtime = append(runtime, map[string]any{"full_name": d, "version": "1.0", "revision": 0, "bottle_rebuild": 0, "pkg_version": "1.0", "declared_directly": true})
	}
	tab, err := json.Marshal(map[string]any{
		"homebrew_version": "6.0.22", "changed_files": b.Changed, "linkage_files": b.Linkage, "binary_relocation_files": []string{},
		"source_modified_time": 1, "compiler": "clang", "runtime_dependencies": runtime, "arch": "arm64",
		"built_on": map[string]any{"os": "Macintosh"},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	manifest := b.Name + "/manifests/" + b.Version
	r.tabs[manifest], r.digests[manifest] = string(tab), digest
	return api.BottleFile{Cellar: cellar, SHA256: digest, URL: r.URL + "/v2/homebrew/core/" + b.Name + "/blobs/sha256:" + digest}
}
