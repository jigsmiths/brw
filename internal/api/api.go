// Package api reads Homebrew's formula metadata from formulae.brew.sh.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const DefaultURL = "https://formulae.brew.sh/api/formula.json"

type Formula struct {
	Name             string                  `json:"name"`
	FullName         string                  `json:"full_name"`
	Tap              string                  `json:"tap"`
	Aliases          []string                `json:"aliases"`
	OldNames         []string                `json:"oldnames"`
	Versions         struct{ Stable string } `json:"versions"`
	Revision         int                     `json:"revision"`
	Bottle           struct{ Stable Bottle } `json:"bottle"`
	Dependencies     []string                `json:"dependencies"`
	KegOnly          bool                    `json:"keg_only"`
	PostInstallSteps []json.RawMessage       `json:"post_install_steps"`
}

type Bottle struct {
	Rebuild int                   `json:"rebuild"`
	RootURL string                `json:"root_url"`
	Files   map[string]BottleFile `json:"files"`
}

type BottleFile struct {
	Cellar string `json:"cellar"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

// BottleTags returns the tags in sorted order.
func (f *Formula) BottleTags() []string {
	tags := make([]string, 0, len(f.Bottle.Stable.Files))
	for tag := range f.Bottle.Stable.Files {
		tags = append(tags, tag)
	}
	slices.Sort(tags)
	return tags
}

func Parse(b []byte) ([]Formula, error) {
	var formulae []Formula
	if err := json.Unmarshal(b, &formulae); err != nil {
		return nil, err
	}
	return formulae, nil
}

// Index finds formulae by name, alias or old name. Real names win over aliases.
type Index struct {
	byName map[string]*Formula
}

func NewIndex(formulae []Formula) *Index {
	idx := &Index{byName: make(map[string]*Formula, len(formulae))}
	for i := range formulae {
		idx.byName[formulae[i].Name] = &formulae[i]
	}
	for i := range formulae {
		for _, alt := range slices.Concat(formulae[i].Aliases, formulae[i].OldNames) {
			if _, taken := idx.byName[alt]; !taken {
				idx.byName[alt] = &formulae[i]
			}
		}
	}
	return idx
}

func (i *Index) Lookup(name string) (*Formula, bool) {
	f, ok := i.byName[name]
	return f, ok
}

// Client fetches the formula listing and caches it in CacheDir by ETag.
type Client struct {
	URL      string
	CacheDir string
	HTTP     *http.Client
}

func (c *Client) Formulae(ctx context.Context) ([]Formula, error) {
	body := filepath.Join(c.CacheDir, "formula.json")
	etagFile := filepath.Join(c.CacheDir, "formula.etag")
	cached, bodyErr := os.ReadFile(body)
	etag, etagErr := os.ReadFile(etagFile)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL, nil)
	if err != nil {
		return nil, err
	}
	if bodyErr == nil && etagErr == nil {
		req.Header.Set("If-None-Match", string(etag))
	}
	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNotModified:
		if bodyErr != nil {
			return nil, fmt.Errorf("304 without a cached body: %w", bodyErr)
		}
		return Parse(cached)
	case http.StatusOK:
	default:
		return nil, fmt.Errorf("GET %s: %s", c.URL, resp.Status)
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	formulae, err := Parse(data)
	if err != nil {
		return nil, err
	}
	if err := writeAtomic(body, data); err != nil {
		return nil, err
	}
	if tag := resp.Header.Get("ETag"); tag != "" {
		err = writeAtomic(etagFile, []byte(tag))
	} else {
		err = os.Remove(etagFile) //fsx:allow cache file
		if errors.Is(err, fs.ErrNotExist) {
			err = nil
		}
	}
	return formulae, err
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //fsx:allow cache dir
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*") //fsx:allow cache file
	if err != nil {
		return err
	}
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp.Name()) //fsx:allow cache file
		return err
	}
	return os.Rename(tmp.Name(), path) //fsx:allow cache file
}
