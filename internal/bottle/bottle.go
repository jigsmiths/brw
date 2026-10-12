// Package bottle downloads bottles from GitHub Packages and checks them.
package bottle

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jigsmiths/brw/internal/api"
)

// auth is the fixed token brew sends to ghcr.io for homebrew/core, which is public.
const auth = "Bearer QQ=="

const defaultWorkers = 4

type Request struct {
	Name       string
	PkgVersion string // version and revision, such as 2.56.0_1
	Rebuild    int
	Tag        string
	File       api.BottleFile
}

type Result struct {
	Path string          // the tarball
	Tab  json.RawMessage // the sh.brew.tab annotation of the bottle's manifest
}

// Fetcher downloads bottles into Dir, keeping them for the next install.
type Fetcher struct {
	HTTP    *http.Client
	Dir     string
	Workers int // simultaneous downloads in FetchAll; 4 if zero
}

// client follows redirects but keeps the token for the origin it was meant for:
// the registry redirects blobs to a CDN, which must not receive it.
func (f *Fetcher) client() *http.Client {
	c := &http.Client{Timeout: 10 * time.Minute}
	if f.HTTP != nil {
		copied := *f.HTTP
		c = &copied
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}
	return c
}

func (r Request) stem() string {
	s := r.Name + "--" + r.PkgVersion + "." + r.Tag + ".bottle"
	if r.Rebuild > 0 {
		s += "." + strconv.Itoa(r.Rebuild)
	}
	return s
}

// Fetch returns the bottle, downloading it unless a copy that still matches its
// checksum is in the cache.
func (f *Fetcher) Fetch(ctx context.Context, req Request) (Result, error) {
	path := filepath.Join(f.Dir, req.stem()+".tar.gz")
	tabPath := filepath.Join(f.Dir, req.stem()+".tab.json")
	if res, ok := f.cached(path, tabPath, req.File.SHA256); ok {
		return res, nil
	}

	tab, err := f.tab(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil { //fsx:allow download cache
		return Result{}, err
	}
	if err := f.download(ctx, req.File, path); err != nil {
		return Result{}, err
	}
	if err := writeAtomic(tabPath, tab); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Tab: tab}, nil
}

func (f *Fetcher) cached(path, tabPath, want string) (Result, bool) {
	tab, err := os.ReadFile(tabPath)
	if err != nil {
		return Result{}, false
	}
	file, err := os.Open(path)
	if err != nil {
		return Result{}, false
	}
	defer func() { _ = file.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil || hex.EncodeToString(h.Sum(nil)) != want {
		return Result{}, false
	}
	return Result{Path: path, Tab: tab}, true
}

// tab finds the manifest of this bottle in the image index and returns its sh.brew.tab.
func (f *Fetcher) tab(ctx context.Context, req Request) (json.RawMessage, error) {
	base, _, ok := strings.Cut(req.File.URL, "/blobs/")
	if !ok {
		return nil, fmt.Errorf("%s: %q is not a bottle blob URL", req.Name, req.File.URL)
	}
	tag := req.PkgVersion
	if req.Rebuild > 0 {
		tag += "-" + strconv.Itoa(req.Rebuild)
	}
	resp, err := f.get(ctx, base+"/manifests/"+tag, "application/vnd.oci.image.index.v1+json")
	if err != nil {
		return nil, fmt.Errorf("%s manifest: %w", req.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var index struct {
		Manifests []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		return nil, fmt.Errorf("%s manifest: %w", req.Name, err)
	}
	for _, m := range index.Manifests {
		if m.Annotations["sh.brew.bottle.digest"] == req.File.SHA256 {
			if tab := m.Annotations["sh.brew.tab"]; tab != "" {
				return json.RawMessage(tab), nil
			}
		}
	}
	return nil, fmt.Errorf("%s: no manifest matches the bottle checksum %s", req.Name, req.File.SHA256)
}

func (f *Fetcher) download(ctx context.Context, file api.BottleFile, path string) error {
	resp, err := f.get(ctx, file.URL, "")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	tmp, err := os.CreateTemp(f.Dir, ".incomplete-*") //fsx:allow download cache
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //fsx:allow download cache; gone after the rename
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), resp.Body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != file.SHA256 {
		return fmt.Errorf("%s: sha256 is %s, want %s", file.URL, got, file.SHA256)
	}
	return os.Rename(tmp.Name(), path) //fsx:allow download cache
}

func (f *Fetcher) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", auth)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	resp, err := f.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp, nil
}

// FetchAll fetches the requests with a few downloads at a time, returning the
// results in request order. If any fail, it returns an error naming each one.
func (f *Fetcher) FetchAll(ctx context.Context, reqs []Request) ([]Result, error) {
	workers := f.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]Result, len(reqs))
	errs := make([]error, len(reqs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			results[i], errs[i] = f.Fetch(ctx, req)
			if errs[i] != nil {
				cancel()
			}
		}()
	}
	wg.Wait()

	var failed []error
	for i, err := range errs {
		if err != nil && !errors.Is(err, context.Canceled) {
			failed = append(failed, fmt.Errorf("%s: %w", reqs[i].Name, err))
		}
	}
	if len(failed) == 0 {
		if err := ctx.Err(); err != nil && errors.Is(err, context.Canceled) && anyFailed(errs) {
			return nil, err
		}
		return results, nil
	}
	return nil, errors.Join(failed...)
}

func anyFailed(errs []error) bool {
	for _, err := range errs {
		if err != nil {
			return true
		}
	}
	return false
}

func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*") //fsx:allow download cache
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) //fsx:allow download cache
		return err
	}
	return os.Rename(tmp.Name(), path) //fsx:allow download cache
}
