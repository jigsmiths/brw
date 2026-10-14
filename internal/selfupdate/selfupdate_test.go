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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

// script is a stand-in for the new binary: it answers `version` like brw does.
func script(version string) string {
	return "#!/bin/sh\necho 'brw " + version + "'\n"
}

func archive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range entries {
		h := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(name, "/") {
			h.Typeflag = tar.TypeDir
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
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

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// release describes what the fake GitHub serves. Zero fields give a good release.
type release struct {
	tag       string            // default v0.2.0
	entries   map[string]string // archive contents, default one brw script printing the tag
	raw       []byte            // replaces the archive when set
	checksum  string            // replaces the checksum of the archive when set
	noSumLine bool              // checksums.txt lists nothing for the archive
	noAsset   bool              // the archive is not among the assets
	noSums    bool              // checksums.txt is not among the assets
	apiStatus int
	apiBody   string
	dlStatus  int
}

type fake struct {
	srv       *httptest.Server
	downloads atomic.Int32
	api       atomic.Int32
}

func serve(t *testing.T, r release) *fake {
	t.Helper()
	if r.tag == "" {
		r.tag = "v0.2.0"
	}
	if r.entries == nil {
		r.entries = map[string]string{"brw": script(strings.TrimPrefix(r.tag, "v")), "LICENSE": "MIT"}
	}
	data := r.raw
	if data == nil {
		data = archive(t, r.entries)
	}
	name := "brw_" + runtime.GOOS + "_" + runtime.GOARCH + ".tar.gz"
	f := &fake{}
	mux := http.NewServeMux()
	mux.HandleFunc("/latest", func(w http.ResponseWriter, _ *http.Request) {
		f.api.Add(1)
		if r.apiStatus != 0 {
			http.Error(w, "boom", r.apiStatus)
			return
		}
		if r.apiBody != "" {
			_, _ = w.Write([]byte(r.apiBody))
			return
		}
		var assets []map[string]string
		if !r.noSums {
			assets = append(assets, map[string]string{"name": "checksums.txt", "browser_download_url": f.srv.URL + "/dl/checksums.txt"})
		}
		if !r.noAsset {
			assets = append(assets, map[string]string{"name": name, "browser_download_url": f.srv.URL + "/dl/" + name})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": r.tag, "assets": assets})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, req *http.Request) {
		f.downloads.Add(1)
		if r.dlStatus != 0 {
			http.Error(w, "boom", r.dlStatus)
			return
		}
		switch strings.TrimPrefix(req.URL.Path, "/dl/") {
		case "checksums.txt":
			digest := sum(data)
			if r.checksum != "" {
				digest = r.checksum
			}
			if r.noSumLine {
				_, _ = fmt.Fprintf(w, "%s  brw_other_other.tar.gz\n", digest)
				return
			}
			_, _ = fmt.Fprintf(w, "%s  brw_other_other.tar.gz\n%s  %s\n", strings.Repeat("0", 64), digest, name)
		case name:
			_, _ = w.Write(data)
		default:
			http.NotFound(w, req)
		}
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// install makes a directory holding the "installed" brw and returns its path.
func install(t *testing.T) string {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "brw")
	if err := os.WriteFile(exe, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	return exe
}

func updater(f *fake, exe, current string) *Updater {
	return &Updater{APIURL: f.srv.URL + "/latest", Exe: exe, Current: current, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}
}

func content(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// unchanged checks the install directory holds the old binary and nothing else.
func unchanged(t *testing.T, exe string) {
	t.Helper()
	if got := content(t, exe); got != "old binary" {
		t.Errorf("the binary was changed: %q", got)
	}
	entries, err := os.ReadDir(filepath.Dir(exe))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Errorf("files left next to the binary: %v", entries)
	}
}

func TestUpdatesToANewerRelease(t *testing.T) {
	f := serve(t, release{})
	exe := install(t)
	res, err := updater(f, exe, "0.1.0").Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.Updated || res.From != "0.1.0" || res.To != "0.2.0" {
		t.Errorf("result = %+v", res)
	}
	if got := content(t, exe); got != script("0.2.0") {
		t.Errorf("binary = %q", got)
	}
	fi, err := os.Stat(exe)
	if err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, %v; want 0755", fi.Mode().Perm(), err)
	}
	entries, _ := os.ReadDir(filepath.Dir(exe))
	if len(entries) != 1 {
		t.Errorf("files left next to the binary: %v", entries)
	}
}

func TestDoesNothingWhenUpToDateOrAhead(t *testing.T) {
	for _, current := range []string{"0.2.0", "0.3.0", "1.0.0"} {
		f := serve(t, release{})
		exe := install(t)
		res, err := updater(f, exe, current).Run(context.Background())
		if err != nil || res.Updated || res.From != current || res.To != current {
			t.Errorf("%s: result = %+v, %v", current, res, err)
		}
		if f.downloads.Load() != 0 {
			t.Errorf("%s: downloaded although nothing was newer", current)
		}
		unchanged(t, exe)
	}
}

func TestNotAReleaseBuild(t *testing.T) {
	f := serve(t, release{})
	exe := install(t)
	if _, err := updater(f, exe, "").Run(context.Background()); !errors.Is(err, ErrNotRelease) {
		t.Errorf("error = %v, want ErrNotRelease", err)
	}
	if f.api.Load() != 0 {
		t.Error("asked GitHub although this is not a release build")
	}
}

func TestLeavesTheBinaryAloneWhenSomethingIsWrong(t *testing.T) {
	huge := `{"tag_name": "v0.2.0", "pad": "` + strings.Repeat("x", maxMetadata) + `"}`
	for name, tt := range map[string]struct {
		r    release
		want string
	}{
		"checksum mismatch":     {release{checksum: strings.Repeat("a", 64)}, "does not match its checksum"},
		"no checksum for it":    {release{noSumLine: true}, "does not list"},
		"no archive for this":   {release{noAsset: true}, "has no brw_"},
		"no checksums at all":   {release{noSums: true}, "has no checksums.txt"},
		"not gzip":              {release{raw: []byte("this is not an archive")}, "gzip"},
		"no brw in the archive": {release{entries: map[string]string{"LICENSE": "MIT"}}, "has no brw"},
		"brw is a directory":    {release{entries: map[string]string{"brw/": ""}}, "has no brw"},
		"wrong version":         {release{entries: map[string]string{"brw": script("9.9.9")}}, "reports"},
		"does not run":          {release{entries: map[string]string{"brw": "#!/bin/sh\nexit 1\n"}}, "does not run"},
		"api error":             {release{apiStatus: http.StatusInternalServerError}, "500"},
		"rate limited":          {release{apiStatus: http.StatusForbidden}, "403"},
		"bad json":              {release{apiBody: "{"}, "latest release"},
		"bad tag":               {release{apiBody: `{"tag_name": "nightly", "assets": []}`}, "not X.Y.Z"},
		"huge description":      {release{apiBody: huge}, "more than"},
		"download error":        {release{dlStatus: http.StatusInternalServerError}, "500"},
	} {
		t.Run(name, func(t *testing.T) {
			f := serve(t, tt.r)
			exe := install(t)
			res, err := updater(f, exe, "0.1.0").Run(context.Background())
			if err == nil || res.Updated || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("result = %+v, %v; want an error with %q", res, err, tt.want)
			}
			unchanged(t, exe)
		})
	}
}

func TestChecksumIsOfTheArchiveNotJustAnyLine(t *testing.T) {
	// A matching digest listed under another file name must not be accepted.
	f := serve(t, release{noSumLine: true})
	exe := install(t)
	if _, err := updater(f, exe, "0.1.0").Run(context.Background()); err == nil {
		t.Fatal("accepted an archive that checksums.txt does not list")
	}
	unchanged(t, exe)
}

func TestRefusesAnOversizedBinary(t *testing.T) {
	old := maxBinary
	maxBinary = 10
	t.Cleanup(func() { maxBinary = old })
	f := serve(t, release{})
	exe := install(t)
	if _, err := updater(f, exe, "0.1.0").Run(context.Background()); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("error = %v, want the size limit", err)
	}
	unchanged(t, exe)
}

func TestUnwritableDirectory(t *testing.T) {
	f := serve(t, release{})
	exe := install(t)
	dir := filepath.Dir(exe)
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if f, err := os.CreateTemp(dir, "probe"); err == nil { // running as root
		_ = f.Close()
		t.Skip("the directory is writable anyway")
	}
	if _, err := updater(f, exe, "0.1.0").Run(context.Background()); err == nil {
		t.Fatal("expected an error")
	}
	if got := content(t, exe); got != "old binary" {
		t.Errorf("binary = %q", got)
	}
}

func TestFindsBrwWhateverTheArchiveLayout(t *testing.T) {
	f := serve(t, release{entries: map[string]string{"brw_0.2.0/brw": script("0.2.0"), "LICENSE": "MIT"}})
	exe := install(t)
	if res, err := updater(f, exe, "0.1.0").Run(context.Background()); err != nil || !res.Updated {
		t.Errorf("result = %+v, %v", res, err)
	}
}

func TestCompare(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.2.0", -1},
		{"0.2.0", "0.2.0", 0},
		{"v0.2.0", "0.2.0", 0},
		{"0.10.0", "0.9.0", 1},
		{"1.0.0", "0.99.99", 1},
		{"0.1.10", "0.1.9", 1},
	} {
		got, err := compare(tt.a, tt.b)
		if err != nil || got != tt.want {
			t.Errorf("compare(%q, %q) = %d, %v; want %d", tt.a, tt.b, got, err, tt.want)
		}
	}
	for _, bad := range []string{"", "1", "1.2", "1.2.x", "a.b.c", "1.2.3-rc1", "1.2.3.4", "1.-2.3"} {
		if _, err := compare(bad, "0.1.0"); err == nil {
			t.Errorf("compare(%q) accepted", bad)
		}
	}
}
