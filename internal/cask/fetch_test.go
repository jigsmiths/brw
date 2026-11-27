package cask

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func server(t *testing.T, body []byte, header http.Header) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/files/tool", http.StatusFound)
			return
		}
		hits.Add(1)
		maps.Copy(w.Header(), header)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func fetch(t *testing.T, f *Fetcher, c api.Cask) string {
	t.Helper()
	path, err := f.Fetch(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestFetchDownloadsOnceAndKeepsIt(t *testing.T) {
	body := []byte("zip bytes")
	srv, hits := server(t, body, nil)
	f := &Fetcher{Dir: t.TempDir()}
	c := api.Cask{Token: "app", URL: srv.URL + "/dl/App-1.0.zip", SHA256: sum(body)}
	path := fetch(t, f, c)
	if want := filepath.Join(f.Dir, sum([]byte(c.URL))+"--App-1.0.zip"); path != want {
		t.Errorf("path = %s, want %s", path, want)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != string(body) {
		t.Errorf("contents = %q, %v", b, err)
	}
	if again := fetch(t, f, c); again != path || hits.Load() != 1 {
		t.Errorf("second fetch = %s after %d downloads, want the cached file", again, hits.Load())
	}
}

func TestFetchChecksumMismatch(t *testing.T) {
	srv, _ := server(t, []byte("tampered"), nil)
	f := &Fetcher{Dir: t.TempDir()}
	want := sum([]byte("original"))
	_, err := f.Fetch(context.Background(), api.Cask{Token: "app", URL: srv.URL + "/App.zip", SHA256: want})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 mismatch") || !strings.Contains(err.Error(), "Expected: "+want) {
		t.Fatalf("err = %v", err)
	}
	if entries, _ := os.ReadDir(f.Dir); len(entries) != 0 {
		t.Errorf("left behind %v", entries)
	}
}

func TestFetchReplacesAStaleDownload(t *testing.T) {
	body := []byte("new")
	srv, hits := server(t, body, nil)
	f := &Fetcher{Dir: t.TempDir()}
	c := api.Cask{Token: "app", URL: srv.URL + "/App.zip", SHA256: sum(body)}
	stale := filepath.Join(f.Dir, sum([]byte(c.URL))+"--Old.zip")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := fetch(t, f, c)
	if hits.Load() != 1 || filepath.Base(path) != sum([]byte(c.URL))+"--App.zip" {
		t.Errorf("path %s after %d downloads", path, hits.Load())
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stale download kept: %v", err)
	}
}

func TestFetchWithoutChecksumDownloadsEachTime(t *testing.T) {
	srv, hits := server(t, []byte("latest"), nil)
	f := &Fetcher{Dir: t.TempDir()}
	c := api.Cask{Token: "app", URL: srv.URL + "/App.zip", SHA256: "no_check"}
	fetch(t, f, c)
	fetch(t, f, c)
	if hits.Load() != 2 {
		t.Errorf("%d downloads, want 2", hits.Load())
	}
}

func TestFetchNamesTheFile(t *testing.T) {
	body := []byte("x")
	cd, _ := server(t, body, http.Header{"Content-Disposition": {`attachment; filename="Real 1.0.dmg"`}})
	plain, _ := server(t, body, nil)
	for _, tc := range []struct{ url, want string }{
		{cd.URL + "/download?id=3", "Real 1.0.dmg"},
		{plain.URL + "/redirect", "tool"},
		{plain.URL + "/a/Notion%20Calendar.zip", "Notion Calendar.zip"},
		{plain.URL + "/", "download"},
	} {
		f := &Fetcher{Dir: t.TempDir()}
		path := fetch(t, f, api.Cask{Token: "app", URL: tc.url, SHA256: sum(body)})
		if got := DownloadName(path); got != tc.want {
			t.Errorf("%s: name %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestFetchRefusesWhatItCannotFetch(t *testing.T) {
	f := &Fetcher{Dir: t.TempDir()}
	for _, c := range []api.Cask{
		{Token: "a", URL: "https://example.com/a.zip", URLSpecs: map[string]json.RawMessage{"user_agent": json.RawMessage(`":fake"`)}},
		{Token: "b", URL: "https://example.com/b.zip", Container: json.RawMessage(`{"type":"naked"}`)},
	} {
		if _, err := f.Fetch(context.Background(), c); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", c.Token, err)
		}
	}
}

func TestFetchFailsOnHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	f := &Fetcher{Dir: t.TempDir()}
	if _, err := f.Fetch(context.Background(), api.Cask{Token: "a", URL: srv.URL + "/a.zip", SHA256: "no_check"}); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v", err)
	}
}

func TestFetchAsksForTheFileUncompressed(t *testing.T) {
	body := []byte("dmg bytes")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write(body)
			_ = gz.Close()
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	var first [2]int64
	f := &Fetcher{Dir: t.TempDir(), Progress: func(_ string, done, total int64) {
		if first == [2]int64{} {
			first = [2]int64{done, total}
		}
	}}
	fetch(t, f, api.Cask{Token: "app", Version: "1.0", URL: srv.URL + "/App.dmg", SHA256: sum(body)})
	if want := int64(len(body)); first[1] != want {
		t.Errorf("first report total = %d, want %d", first[1], want)
	}
}

func TestProgressOfUnknownSizeEndsComplete(t *testing.T) {
	var last [2]int64
	p := &progressReader{r: strings.NewReader("bytes"), total: -1, report: func(_ string, done, total int64) {
		last = [2]int64{done, total}
	}}
	if _, err := io.ReadAll(p); err != nil {
		t.Fatal(err)
	}
	if last != [2]int64{5, 5} {
		t.Errorf("last report = done %d of %d, want 5 of 5", last[0], last[1])
	}
}
