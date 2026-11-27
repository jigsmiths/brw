// Package bottle downloads bottles, from GitHub Packages or a tap's own
// root_url, and checks them.
package bottle

import (
	"archive/tar"
	"compress/gzip"
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
	"github.com/jigsmiths/brw/internal/progress"
)

// auth is the fixed token brew sends to ghcr.io for homebrew/core, which is public.
const auth = "Bearer QQ=="

const workers = 4

type Request struct {
	Name       string
	PkgVersion string // version and revision, such as 2.56.0_1
	Rebuild    int
	Tag        string
	File       api.BottleFile
}

type Result struct {
	Path           string
	Tab            json.RawMessage // the sh.brew.tab annotation of the bottle's manifest
	SBOMSupplement json.RawMessage
}

type Fetcher struct {
	HTTP     *http.Client
	Dir      string
	Progress func(label string, done int64, total progress.Size)
}

type progressReader struct {
	r      io.Reader
	label  string
	done   int64
	total  progress.Size
	report func(label string, done int64, total progress.Size)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	if err == io.EOF {
		p.total = progress.Known(p.done)
	}
	p.report(p.label, p.done, p.total)
	return n, err
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

// Manifest annotations are cached beside the bottle, so a cached bottle needs no request.
func (f *Fetcher) Fetch(ctx context.Context, req Request) (Result, error) {
	stem := filepath.Join(f.Dir, req.stem())
	path, tabPath, suppPath := stem+".tar.gz", stem+".tab.json", stem+".sbom.json"
	haveBottle := verified(path, req.File.SHA256)
	if haveBottle {
		tab, errTab := os.ReadFile(tabPath)
		supp, errSupp := os.ReadFile(suppPath)
		if errTab == nil && errSupp == nil {
			return Result{Path: path, Tab: tab, SBOMSupplement: supp}, nil
		}
	}

	if !strings.Contains(req.File.URL, "/blobs/") {
		return f.fetchPlain(ctx, req, path, tabPath, suppPath, haveBottle)
	}
	ann, err := f.annotations(ctx, req)
	if err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil { //fsx:allow download cache
		return Result{}, err
	}
	if !haveBottle {
		if err := f.download(ctx, req.Name+" ("+req.PkgVersion+")", req.File, path, verify); err != nil {
			return Result{}, err
		}
	}
	if err := writeAtomic(tabPath, ann.tab); err != nil {
		return Result{}, err
	}
	if err := writeAtomic(suppPath, ann.supplement); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Tab: ann.tab, SBOMSupplement: ann.supplement}, nil
}

func (f *Fetcher) FetchSource(ctx context.Context, name, version, url, sha256 string) (string, error) {
	if sha256 == "" {
		return "", fmt.Errorf("%s has no checksum to verify", url)
	}
	path := f.sourcePath(name, version, url)
	if verified(path, sha256) {
		return path, nil
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil { //fsx:allow download cache
		return "", err
	}
	return path, f.download(ctx, name+" ("+version+")", api.BottleFile{URL: url, SHA256: sha256}, path, verify)
}

func (f *Fetcher) FetchUnverified(ctx context.Context, name, version, url string) (string, error) {
	path := f.sourcePath(name, version, url)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil { //fsx:allow download cache
		return "", err
	}
	return path, f.download(ctx, name+" ("+version+")", api.BottleFile{URL: url}, path, skipVerify)
}

func (f *Fetcher) sourcePath(name, version, url string) string {
	base := url[strings.LastIndex(url, "/")+1:]
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	return filepath.Join(f.Dir, name+"--"+version+"--"+base)
}

// A tap's root_url may serve bottles outside a registry: no manifest, so the
// tab is the bottle's own INSTALL_RECEIPT.json, and no token is sent.
func (f *Fetcher) fetchPlain(ctx context.Context, req Request, path, tabPath, suppPath string, haveBottle bool) (Result, error) {
	if err := os.MkdirAll(f.Dir, 0o755); err != nil { //fsx:allow download cache
		return Result{}, err
	}
	if !haveBottle {
		if err := f.download(ctx, req.Name+" ("+req.PkgVersion+")", req.File, path, verify); err != nil {
			return Result{}, err
		}
	}
	tab, err := tabInBottle(path, req.Name+"/"+req.PkgVersion+"/INSTALL_RECEIPT.json")
	if err != nil {
		return Result{}, fmt.Errorf("%s: %w", req.Name, err)
	}
	if err := writeAtomic(tabPath, tab); err != nil {
		return Result{}, err
	}
	if err := writeAtomic(suppPath, nil); err != nil {
		return Result{}, err
	}
	return Result{Path: path, Tab: tab}, nil
}

func tabInBottle(path, name string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("the bottle has no %s", name)
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(h.Name, "./") == name {
			return io.ReadAll(io.LimitReader(tr, 1<<20))
		}
	}
}

func verified(path, want string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, file); err != nil {
		return false
	}
	return hex.EncodeToString(h.Sum(nil)) == want
}

type annotations struct {
	tab        json.RawMessage // sh.brew.tab
	supplement json.RawMessage // sh.brew.sbom.supplement, possibly empty
}

func (f *Fetcher) annotations(ctx context.Context, req Request) (annotations, error) {
	base, _, ok := strings.Cut(req.File.URL, "/blobs/")
	if !ok {
		return annotations{}, fmt.Errorf("%s: %q is not a bottle blob URL", req.Name, req.File.URL)
	}
	tag := req.PkgVersion
	if req.Rebuild > 0 {
		tag += "-" + strconv.Itoa(req.Rebuild)
	}
	resp, err := f.get(ctx, base+"/manifests/"+tag, "application/vnd.oci.image.index.v1+json")
	if err != nil {
		return annotations{}, fmt.Errorf("%s manifest: %w", req.Name, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var index struct {
		Manifests []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&index); err != nil {
		return annotations{}, fmt.Errorf("%s manifest: %w", req.Name, err)
	}
	for _, m := range index.Manifests {
		if m.Annotations["sh.brew.bottle.digest"] == req.File.SHA256 {
			if tab := m.Annotations["sh.brew.tab"]; tab != "" {
				return annotations{tab: json.RawMessage(tab), supplement: json.RawMessage(m.Annotations["sh.brew.sbom.supplement"])}, nil
			}
		}
	}
	return annotations{}, fmt.Errorf("%s: no manifest matches the bottle checksum %s", req.Name, req.File.SHA256)
}

type verification int

const (
	verify     verification = iota
	skipVerify              // a head archive, which has no checksum
)

func (f *Fetcher) download(ctx context.Context, name string, file api.BottleFile, path string, check verification) error {
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
	var body io.Reader = resp.Body
	if f.Progress != nil {
		body = &progressReader{r: body, label: name, total: progress.SizeOf(resp.ContentLength), report: f.Progress}
	}
	_, err = io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); check == verify && got != file.SHA256 {
		return fmt.Errorf("%s: sha256 is %s, want %s", file.URL, got, file.SHA256)
	}
	// keep the server's time, as curl --remote-time does for brew
	if when, err := http.ParseTime(resp.Header.Get("Last-Modified")); err == nil {
		_ = os.Chtimes(tmp.Name(), when, when) //fsx:allow download cache
	}
	return os.Rename(tmp.Name(), path) //fsx:allow download cache
}

func (f *Fetcher) get(ctx context.Context, url, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if strings.Contains(url, "/blobs/") || strings.Contains(url, "/manifests/") {
		req.Header.Set("Authorization", auth)
	}
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	c := f.client()
	if req.URL.Scheme == "file" {
		c = &http.Client{Transport: http.NewFileTransport(http.Dir("/"))}
	}
	resp, err := c.Do(req)
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]Result, len(reqs))
	errs := make([]error, len(reqs))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, req := range reqs {
		wg.Go(func() {
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
		})
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
