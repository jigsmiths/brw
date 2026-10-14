// Package selfupdate replaces the running brw with the latest release.
package selfupdate

import (
	"archive/tar"
	"bytes"
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
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultAPIURL is where the latest release of brw is described.
const DefaultAPIURL = "https://api.github.com/repos/jigsmiths/brw/releases/latest"

// ErrNotRelease means this brw was not installed from a release, so there is
// nothing to compare with.
var ErrNotRelease = errors.New("not installed from a release")

const (
	maxMetadata = 1 << 20   // the release description and checksums.txt
	maxArchive  = 200 << 20 // the downloaded archive
)

// maxBinary caps the brw extracted from the archive. It is a variable for tests.
var maxBinary int64 = 200 << 20

// Updater brings the binary at Exe up to the latest release.
type Updater struct {
	APIURL       string // the latest release; DefaultAPIURL when empty
	HTTP         *http.Client
	Exe          string // the binary to replace
	Current      string // its release version, such as 0.1.0
	GOOS, GOARCH string
}

// Result says what Run did.
type Result struct {
	From, To string
	Updated  bool
}

// Run replaces Exe when the latest release is newer than Current. It changes
// nothing unless the new binary checked out: the archive must match checksums.txt
// and the new binary must report the release's version.
func (u *Updater) Run(ctx context.Context) (Result, error) {
	if u.Current == "" {
		return Result{}, ErrNotRelease
	}
	tag, assets, err := u.latest(ctx)
	if err != nil {
		return Result{}, err
	}
	latest := strings.TrimPrefix(tag, "v")
	cmp, err := compare(latest, u.Current)
	if err != nil {
		return Result{}, err
	}
	if cmp <= 0 {
		return Result{From: u.Current, To: u.Current}, nil
	}
	if err := u.install(ctx, latest, assets); err != nil {
		return Result{}, err
	}
	return Result{From: u.Current, To: latest, Updated: true}, nil
}

func (u *Updater) latest(ctx context.Context) (tag string, assets map[string]string, err error) {
	url := u.APIURL
	if url == "" {
		url = DefaultAPIURL
	}
	data, err := u.get(ctx, url, maxMetadata)
	if err != nil {
		return "", nil, err
	}
	var rel struct {
		Tag    string `json:"tag_name"`
		Assets []struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}
	}
	if err := json.Unmarshal(data, &rel); err != nil {
		return "", nil, fmt.Errorf("the latest release: %w", err)
	}
	assets = map[string]string{}
	for _, a := range rel.Assets {
		assets[a.Name] = a.URL
	}
	return rel.Tag, assets, nil
}

func (u *Updater) install(ctx context.Context, version string, assets map[string]string) error {
	name := "brw_" + u.GOOS + "_" + u.GOARCH + ".tar.gz"
	archiveURL, ok := assets[name]
	if !ok {
		return fmt.Errorf("release %s has no %s", version, name)
	}
	sumsURL, ok := assets["checksums.txt"]
	if !ok {
		return fmt.Errorf("release %s has no checksums.txt", version)
	}
	sums, err := u.get(ctx, sumsURL, maxMetadata)
	if err != nil {
		return err
	}
	want, ok := checksum(string(sums), name)
	if !ok {
		return fmt.Errorf("checksums.txt does not list %s", name)
	}
	data, err := u.get(ctx, archiveURL, maxArchive)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s does not match its checksum", name)
	}

	tmp, err := os.CreateTemp(filepath.Dir(u.Exe), ".brw-update-*") //fsx:allow brw's own binary, not the prefix
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }() //fsx:allow brw's own binary; gone after the rename, cleans up after a failure
	err = extract(data, tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil { //fsx:allow brw's own binary
		return err
	}
	out, err := exec.CommandContext(ctx, tmp.Name(), "version").Output()
	if err != nil {
		return fmt.Errorf("the new brw does not run: %w", err)
	}
	if got := strings.TrimSpace(string(out)); got != "brw "+version {
		return fmt.Errorf("the new brw reports %q, want %q", got, "brw "+version)
	}
	return os.Rename(tmp.Name(), u.Exe) //fsx:allow brw's own binary
}

// get fetches url, refusing more than limit bytes.
func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "brw")
	hc := u.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 2 * time.Minute}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("GET %s: more than %d bytes", url, limit)
	}
	return data, nil
}

// checksum finds the digest listed for name in a checksums.txt.
func checksum(sums, name string) (string, bool) {
	for _, line := range strings.Split(sums, "\n") {
		digest, file, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok && strings.TrimLeft(file, " *") == name {
			return strings.ToLower(digest), true
		}
	}
	return "", false
}

// extract copies the brw entry of a tar.gz to w.
func extract(archive []byte, w io.Writer) error {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return errors.New("the archive has no brw")
		}
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg || path.Base(h.Name) != "brw" {
			continue
		}
		n, err := io.Copy(w, io.LimitReader(tr, maxBinary+1))
		if err != nil {
			return err
		}
		if n > maxBinary {
			return fmt.Errorf("brw in the archive is larger than %d bytes", maxBinary)
		}
		return nil
	}
}

// compare orders two X.Y.Z versions, with or without a leading v.
func compare(a, b string) (int, error) {
	x, err := parse(a)
	if err != nil {
		return 0, err
	}
	y, err := parse(b)
	if err != nil {
		return 0, err
	}
	for i := range x {
		if x[i] != y[i] {
			if x[i] < y[i] {
				return -1, nil
			}
			return 1, nil
		}
	}
	return 0, nil
}

func parse(v string) ([3]int, error) {
	var out [3]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != 3 {
		return out, fmt.Errorf("version %q is not X.Y.Z", v)
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, fmt.Errorf("version %q is not X.Y.Z", v)
		}
		out[i] = n
	}
	return out, nil
}
