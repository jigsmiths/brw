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
	"sort"
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

type Bottle struct {
	Name, Version string
	Files         []File
	Changed       []string // tab changed_files
	Linkage       []string // tab linkage_files
	Deps          []string // tab runtime_dependencies
	// SBOMSupplement is the sh.brew.sbom.supplement manifest annotation.
	SBOMSupplement string
}

// Registry redirects a blob to a second server, as ghcr.io does.
type Registry struct {
	t         testing.TB
	URL       string
	blobs     map[string][]byte // by digest
	tabs      map[string]string // by manifest path
	supps     map[string]string // by manifest path
	digests   map[string]string
	tampered  map[string]bool
	plain     map[string]string // digests by file name under /plain/
	manifests atomic.Int32
}

func NewRegistry(t testing.TB) *Registry {
	t.Helper()
	r := &Registry{t: t, blobs: map[string][]byte{}, tabs: map[string]string{}, supps: map[string]string{}, digests: map[string]string{}, tampered: map[string]bool{}, plain: map[string]string{}}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		digest := strings.TrimPrefix(req.URL.Path, "/blob/")
		body := r.blobs[digest]
		if r.tampered[digest] {
			body = append([]byte("x"), body...)
		}
		_, _ = w.Write(body)
	}))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if file, ok := strings.CutPrefix(req.URL.Path, "/plain/"); ok {
			if digest, ok := r.plain[file]; ok {
				_, _ = w.Write(r.blobs[digest])
				return
			}
			http.NotFound(w, req)
			return
		}
		path := strings.TrimPrefix(req.URL.Path, "/v2/homebrew/core/")
		switch {
		case strings.Contains(path, "/manifests/"):
			r.manifests.Add(1)
			annotations := map[string]string{"sh.brew.bottle.digest": r.digests[path], "sh.brew.tab": r.tabs[path]}
			if supp := r.supps[path]; supp != "" {
				annotations["sh.brew.sbom.supplement"] = supp
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []map[string]any{{"annotations": annotations}}})
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

// Tamper serves other bytes than the checksum says.
func (r *Registry) Tamper(file api.BottleFile) { r.tampered[file.SHA256] = true }

func (r *Registry) ManifestRequests() int { return int(r.manifests.Load()) }

func (r *Registry) Add(b Bottle, cellar string) api.BottleFile {
	r.t.Helper()
	tab := r.tab(b)
	digest := r.store(r.tarball(b, ""))
	manifest := b.Name + "/manifests/" + b.Version
	r.tabs[manifest], r.digests[manifest], r.supps[manifest] = string(tab), digest, b.SBOMSupplement
	return api.BottleFile{Cellar: cellar, SHA256: digest, URL: r.URL + "/v2/homebrew/core/" + b.Name + "/blobs/sha256:" + digest}
}

// AddPlain serves a bottle with no manifest, as a tap's root_url outside GitHub
// Packages does; the tab is the bottle's own INSTALL_RECEIPT.json.
func (r *Registry) AddPlain(b Bottle, tag, cellar string) api.BottleFile {
	r.t.Helper()
	digest := r.store(r.tarball(b, string(r.tab(b))))
	file := b.Name + "-" + b.Version + "." + tag + ".bottle.tar.gz"
	r.plain[file] = digest
	return api.BottleFile{Cellar: cellar, SHA256: digest, URL: r.URL + "/plain/" + file}
}

func (r *Registry) AddSource(file string, body []byte) (url, sha256 string) {
	digest := r.store(body)
	r.plain[file] = digest
	return r.URL + "/plain/" + file, digest
}

// Tarball makes a body starting with #! executable.
func Tarball(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		mode := int64(0o644)
		if strings.HasPrefix(files[n], "#!") {
			mode = 0o755
		}
		if err := tw.WriteHeader(&tar.Header{Name: n, Mode: mode, Size: int64(len(files[n])), ModTime: modTime, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(files[n])); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func (r *Registry) PlainRoot() string { return r.URL + "/plain" }

func (r *Registry) store(body []byte) string {
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	r.blobs[digest] = body
	return digest
}

func (r *Registry) tab(b Bottle) []byte {
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
	return tab
}

func (r *Registry) tarball(b Bottle, receipt string) []byte {
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
	if receipt != "" {
		write(keg+"/INSTALL_RECEIPT.json", tar.TypeReg, receipt, 0o644)
	}
	if err := tw.Close(); err != nil {
		r.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		r.t.Fatal(err)
	}
	return buf.Bytes()
}
