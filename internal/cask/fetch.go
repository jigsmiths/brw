// Package cask downloads casks and unpacks them the way brew does.
package cask

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
)

// ErrUnsupported marks a cask, or a download, that brw cannot handle yet.
var ErrUnsupported = errors.New("not supported by brw yet")

// noCheck is the sha256 of a cask whose download is not checked.
const noCheck = "no_check"

// Fetcher downloads casks into Dir and keeps them for the next install.
type Fetcher struct {
	HTTP *http.Client
	Dir  string
	// Progress, if set, is told how much of a download has arrived, and how
	// much there is in all (-1 if unknown).
	Progress func(name string, done, total int64)
}

// Fetch returns the cask's download, from the cache when it is there and has
// the checksum. A cask without a checksum is downloaded each time.
func (f *Fetcher) Fetch(ctx context.Context, c api.Cask) (string, error) {
	if len(c.URLSpecs) > 0 {
		return "", fmt.Errorf("%s: downloads with url options: %w", c.Token, ErrUnsupported)
	}
	if len(c.Container) > 0 && string(c.Container) != "null" {
		return "", fmt.Errorf("%s: container %s: %w", c.Token, c.Container, ErrUnsupported)
	}
	if c.URL == "" {
		return "", fmt.Errorf("%s has no download for this machine", c.Token)
	}
	key := sum256(c.URL)
	cached, err := filepath.Glob(filepath.Join(f.Dir, key+"--*"))
	if err != nil {
		return "", err
	}
	if c.SHA256 != noCheck && len(cached) == 1 && hasSum(cached[0], c.SHA256) {
		return cached[0], nil
	}
	path, err := f.download(ctx, c, key)
	if err != nil {
		return "", err
	}
	for _, old := range cached {
		if old != path {
			_ = os.Remove(old) //fsx:allow download cache
		}
	}
	return path, quarantine(ctx, path)
}

// DownloadName is the download's own name, as it is in the cache after the
// key, which is how brew names a file it copies out unchanged.
func DownloadName(path string) string {
	base := filepath.Base(path)
	if key, name, ok := strings.Cut(base, "--"); ok && len(key) == 64 {
		return name
	}
	return base
}

func (f *Fetcher) download(ctx context.Context, c api.Cask, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return "", err
	}
	client := f.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download failed on cask '%s': %w", c.Token, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download failed on cask '%s': GET %s: %s", c.Token, c.URL, resp.Status)
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil { //fsx:allow download cache
		return "", err
	}
	tmp, err := os.CreateTemp(f.Dir, ".incomplete-*") //fsx:allow download cache
	if err != nil {
		return "", err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //fsx:allow download cache; gone after the rename
	h := sha256.New()
	var body io.Reader = resp.Body
	if f.Progress != nil {
		body = &progressReader{r: body, name: c.Token + " (" + c.Version + ")", total: resp.ContentLength, report: f.Progress}
	}
	_, err = io.Copy(io.MultiWriter(tmp, h), body)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download failed on cask '%s': %w", c.Token, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); c.SHA256 != noCheck && got != c.SHA256 {
		return "", fmt.Errorf("%s: SHA-256 mismatch\nExpected: %s\n  Actual: %s\n     URL: %s", c.Token, c.SHA256, got, c.URL)
	}
	path := filepath.Join(f.Dir, key+"--"+downloadName(resp))
	return path, os.Rename(tmp.Name(), path) //fsx:allow download cache
}

var unsafeName = regexp.MustCompile(`[\x00-\x1f\x7f/]`)

// downloadName is the name the server gives the file, or else the last part of
// the URL after redirects.
func downloadName(resp *http.Response) string {
	name := ""
	if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
		name = params["filename"]
	}
	if name == "" {
		name = path.Base(resp.Request.URL.Path)
	}
	name = unsafeName.ReplaceAllString(name, "")
	if name == "" || name == "." || name == ".." {
		return "download"
	}
	return name
}

func sum256(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hasSum(path, want string) bool {
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

type progressReader struct {
	r           io.Reader
	name        string
	done, total int64
	report      func(name string, done, total int64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.done += int64(n)
	p.report(p.name, p.done, p.total)
	return n, err
}
