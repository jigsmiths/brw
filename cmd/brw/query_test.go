package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/platform"
)

var linux = platform.Host{OS: "linux", Arch: "amd64"}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// apiServer serves the formula fixture and returns a client with a fresh cache.
func apiServer(t *testing.T) *api.Client {
	t.Helper()
	body := readFile(t, "../../internal/api/testdata/formulae.json")
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(ts.Close)
	return &api.Client{URL: ts.URL, CacheDir: t.TempDir()}
}

// prefixWith installs keg receipts from the cellar fixtures into a new prefix.
func prefixWith(t *testing.T, kegs map[string]string) string {
	t.Helper()
	prefix := t.TempDir()
	for keg, receipt := range kegs {
		name, version, _ := strings.Cut(keg, "@")
		writeFile(t, filepath.Join(prefix, "Cellar", name, version, "INSTALL_RECEIPT.json"),
			readFile(t, "../../internal/cellar/testdata/"+receipt+".json"))
	}
	return prefix
}

func runCmd(t *testing.T, e *env, cmd string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	e.out = &out
	if e.err == nil {
		e.err = &bytes.Buffer{}
	}
	err := run(e, cmd, args)
	return out.String(), err
}

func TestList(t *testing.T) {
	prefix := prefixWith(t, map[string]string{"jq@1.8.2": "jq", "age@1.0": "age"})
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	e := &env{host: linux}
	if got, err := runCmd(t, e, "list", "-prefix", prefix); err != nil || got != "age\nempty\njq\n" {
		t.Errorf("list = %q, %v", got, err)
	}
	if got, err := runCmd(t, e, "list", "-versions", "-prefix", prefix); err != nil || got != "age 1.0\njq 1.8.2\n" {
		t.Errorf("list --versions = %q, %v", got, err)
	}
}

func TestSearch(t *testing.T) {
	e := &env{host: linux, client: apiServer(t)}
	if got, err := runCmd(t, e, "search", "json"); err != nil || got != "json-c\n" {
		t.Errorf("search = %q, %v", got, err)
	}
	if got, err := runCmd(t, e, "search", "/^lib/"); err != nil || got != "libmpdec\nlibunistring\n" {
		t.Errorf("search regex = %q, %v", got, err)
	}
	want := "==> Formulae\njq: Lightweight and flexible command-line JSON processor\njson-c: JSON parser for C\n"
	if got, err := runCmd(t, e, "search", "--desc", "json"); err != nil || got != want {
		t.Errorf("search --desc = %q, %v", got, err)
	}
	if got, err := runCmd(t, e, "search", "--desc", "zzzqqq"); err != nil || got != "==> Formulae\n" {
		t.Errorf("search --desc without matches = %q, %v", got, err)
	}
	if _, err := runCmd(t, e, "search", "zzzqqq"); err == nil {
		t.Error("a name search without matches should fail")
	}
	if _, err := runCmd(t, e, "search"); err == nil {
		t.Error("search needs a query")
	}
}

func TestDeps(t *testing.T) {
	e := &env{host: linux, client: apiServer(t)}
	prefix := prefixWith(t, map[string]string{"jq@1.8.2": "jq"})
	flags := []string{"-prefix", prefix}

	// An installed formula reports the receipt's runtime dependencies.
	if got, err := runCmd(t, e, "deps", append([]string{"jq"}, flags...)...); err != nil || got != "oniguruma\n" {
		t.Errorf("deps jq = %q, %v", got, err)
	}
	// An uninstalled one is resolved from the API.
	if got, err := runCmd(t, e, "deps", append([]string{"--1", "openssl@3"}, flags...)...); err != nil || got != "ca-certificates\n" {
		t.Errorf("deps --1 openssl@3 = %q, %v", got, err)
	}
	// Flags may follow the name, and a tree ends with a blank line.
	got, err := runCmd(t, e, "deps", append([]string{"openssl@3", "--tree"}, flags...)...)
	if err != nil || got != "openssl@3\n└── ca-certificates\n\n" {
		t.Errorf("deps openssl@3 --tree = %q, %v", got, err)
	}
	if got, err := runCmd(t, e, "deps", append([]string{"openssl@3"}, flags...)...); err != nil || got != "ca-certificates\n" {
		t.Errorf("deps openssl@3 = %q, %v", got, err)
	}
	if _, err := runCmd(t, e, "deps", append([]string{"--installed", "openssl@3"}, flags...)...); err == nil {
		t.Error("--installed on an uninstalled formula should fail")
	}
	if _, err := runCmd(t, e, "deps", append([]string{"nosuchformula", "--1"}, flags...)...); err == nil {
		t.Error("unknown formula should fail")
	}
}

func TestUsesInstalled(t *testing.T) {
	e := &env{host: linux}
	prefix := prefixWith(t, map[string]string{"jq@1.8.2": "jq", "age@1.0": "age"})
	if got, err := runCmd(t, e, "uses", "--installed", "oniguruma", "-prefix", prefix); err != nil || got != "jq\n" {
		t.Errorf("uses --installed = %q, %v", got, err)
	}
	if got, err := runCmd(t, e, "uses", "--installed", "nothing", "-prefix", prefix); err != nil || got != "" {
		t.Errorf("uses --installed with no dependents = %q, %v", got, err)
	}
}

func TestOutdated(t *testing.T) {
	prefix := prefixWith(t, map[string]string{"jq@1.0": "jq"})
	flags := []string{"-prefix", prefix}
	e := &env{host: linux, client: apiServer(t)}

	if got, err := runCmd(t, e, "outdated", flags...); err != nil || got != "jq\n" {
		t.Errorf("outdated = %q, %v", got, err)
	}
	if got, _ := runCmd(t, e, "outdated", append([]string{"-v"}, flags...)...); got != "jq (1.0) < 1.8.2\n" {
		t.Errorf("outdated -v = %q", got)
	}
	tty := &env{host: linux, client: e.client, tty: true}
	if got, _ := runCmd(t, tty, "outdated", flags...); got != "jq (1.0) < 1.8.2\n" {
		t.Errorf("outdated on a terminal = %q", got)
	}
	if got, _ := runCmd(t, tty, "outdated", append([]string{"-q"}, flags...)...); got != "jq\n" {
		t.Errorf("outdated -q on a terminal = %q", got)
	}

	wantJSON := `{
  "formulae": [
    {
      "name": "jq",
      "installed_versions": [
        "1.0"
      ],
      "current_version": "1.8.2",
      "pinned": false,
      "pinned_version": null
    }
  ],
  "casks": []
}
`
	if got, _ := runCmd(t, e, "outdated", append([]string{"--json"}, flags...)...); got != wantJSON {
		t.Errorf("outdated --json =\n%s", got)
	}

	if err := os.MkdirAll(filepath.Join(prefix, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/jq/1.0", filepath.Join(prefix, "var/homebrew/pinned/jq")); err != nil {
		t.Fatal(err)
	}
	if got, _ := runCmd(t, e, "outdated", append([]string{"-v"}, flags...)...); got != "jq (1.0) < 1.8.2 [pinned at 1.0]\n" {
		t.Errorf("pinned outdated -v = %q", got)
	}
	if got, _ := runCmd(t, e, "outdated", append([]string{"--json"}, flags...)...); !strings.Contains(got, `"pinned_version": "1.0"`) {
		t.Errorf("pinned outdated --json = %s", got)
	}
}

func TestOutdatedNothing(t *testing.T) {
	e := &env{host: linux, client: apiServer(t)}
	prefix := t.TempDir()
	if got, err := runCmd(t, e, "outdated", "-prefix", prefix); err != nil || got != "" {
		t.Errorf("outdated = %q, %v", got, err)
	}
	want := "{\n  \"formulae\": [],\n  \"casks\": []\n}\n"
	if got, _ := runCmd(t, e, "outdated", "--json", "-prefix", prefix); got != want {
		t.Errorf("outdated --json =\n%s", got)
	}
}

func TestOfflineWarnsOnStderr(t *testing.T) {
	c := apiServer(t)
	e := &env{host: linux, client: c}
	if _, err := runCmd(t, e, "search", "json"); err != nil {
		t.Fatal(err)
	}
	later := func() time.Time { return time.Now().Add(time.Hour) }
	offline := &env{host: linux, client: &api.Client{URL: "http://127.0.0.1:1", CacheDir: c.CacheDir, Now: later}, err: &bytes.Buffer{}}
	got, err := runCmd(t, offline, "search", "json")
	if err != nil || got != "json-c\n" {
		t.Fatalf("offline search = %q, %v", got, err)
	}
	if w := offline.err.(*bytes.Buffer).String(); !strings.HasPrefix(w, "brw: warning: using the cached formula list") {
		t.Errorf("stderr = %q", w)
	}
}

// linkKeg points opt/NAME at the keg, so it counts as linked.
func linkKeg(t *testing.T, prefix, name, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(prefix, "opt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/"+name+"/"+version, filepath.Join(prefix, "opt", name)); err != nil {
		t.Fatal(err)
	}
}

func TestInfo(t *testing.T) {
	prefix := prefixWith(t, map[string]string{"jq@1.8.2": "jq"})
	linkKeg(t, prefix, "jq", "1.8.2")
	e := &env{host: linux, client: apiServer(t)}
	flags := []string{"-prefix", prefix}

	got, err := runCmd(t, e, "info", append([]string{"jq"}, flags...)...)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"==> jq: stable 1.8.2 (bottled), HEAD\n",
		"Installed (on request)\n",
		"From: https://github.com/Homebrew/homebrew-core/blob/HEAD/Formula/j/jq.rb\n",
		"==> Installed Versions\njq 1.8.2 (",
		"==> Dependencies\nRequired (1): oniguruma\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("info jq is missing %q:\n%s", want, got)
		}
	}

	// Several formulae are separated by a blank line.
	got, err = runCmd(t, e, "info", append([]string{"zsh-lovers", "xz"}, flags...)...)
	if err != nil || !strings.Contains(got, "\n\n==> xz: ") {
		t.Errorf("info of two = %q, %v", got, err)
	}
	if _, err := runCmd(t, e, "info", append([]string{"nosuchformula"}, flags...)...); err == nil {
		t.Error("an unknown formula should fail")
	}
	if _, err := runCmd(t, e, "info"); err == nil {
		t.Error("info needs a formula")
	}
}

func TestInfoJSON(t *testing.T) {
	prefix := prefixWith(t, map[string]string{"jq@1.8.2": "jq"})
	linkKeg(t, prefix, "jq", "1.8.2")
	e := &env{host: linux, client: apiServer(t)}
	got, err := runCmd(t, e, "info", "--json", "jq", "-prefix", prefix)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Formulae []struct {
			Name      string            `json:"name"`
			Installed []json.RawMessage `json:"installed"`
			Bottle    struct {
				Stable struct {
					Files map[string]json.RawMessage `json:"files"`
				} `json:"stable"`
			} `json:"bottle"`
		} `json:"formulae"`
		Casks []any `json:"casks"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("%v\n%s", err, got)
	}
	if len(doc.Formulae) != 1 || doc.Formulae[0].Name != "jq" || len(doc.Formulae[0].Installed) != 1 || doc.Casks == nil || len(doc.Casks) != 0 {
		t.Errorf("unexpected document:\n%s", got)
	}
	if files := doc.Formulae[0].Bottle.Stable.Files; len(files) != 1 || files["x86_64_linux"] == nil {
		t.Errorf("bottle files = %v, want only this host's", files)
	}
	if !strings.HasPrefix(got, "{\n  \"formulae\": [\n    {\n      \"name\": \"jq\",\n") || !strings.HasSuffix(got, "  ],\n  \"casks\": []\n}\n") {
		t.Errorf("not pretty printed like brew:\n%.200s", got)
	}
}
