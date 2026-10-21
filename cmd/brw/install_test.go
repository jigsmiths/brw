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

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/platform"
)

var mac = platform.Host{OS: "darwin", MacVersion: "26.0", Arch: "arm64"}

// store is a formula API and a registry with the bottles it lists.
type store struct {
	t        *testing.T
	reg      *installtest.Registry
	formulae []map[string]any
}

// formula adds a formula with a bottle for the Mac tag. extra overrides API keys.
func (s *store) formula(b installtest.Bottle, deps []string, extra map[string]any) {
	s.t.Helper()
	file := s.reg.Add(b, ":any")
	m := map[string]any{
		"name": b.Name, "full_name": b.Name, "tap": "homebrew/core", "desc": b.Name + " description",
		"versions": map[string]any{"stable": b.Version}, "dependencies": deps, "aliases": []string{},
		"bottle": map[string]any{"stable": map[string]any{"rebuild": 0, "files": map[string]any{
			"arm64_tahoe": map[string]any{"cellar": file.Cellar, "url": file.URL, "sha256": file.SHA256},
		}}},
	}
	for k, v := range extra {
		m[k] = v
	}
	s.formulae = append(s.formulae, m)
}

// env serves the formulae and returns an env with an empty prefix.
func (s *store) env() (*env, string) {
	s.t.Helper()
	body, err := json.Marshal(s.formulae)
	if err != nil {
		s.t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	s.t.Cleanup(ts.Close)
	return &env{host: mac, client: &api.Client{URL: ts.URL, CacheDir: s.t.TempDir()}, err: &bytes.Buffer{}}, s.t.TempDir()
}

func newStore(t *testing.T) *store { return &store{t: t, reg: installtest.NewRegistry(t)} }

func file(name, body string, mode int64) installtest.File {
	return installtest.File{Name: name, Body: body, Mode: mode}
}

func jqBottle() installtest.Bottle {
	return installtest.Bottle{Name: "jq", Version: "1.0", Files: []installtest.File{
		file("bin/jq", "#!/bin/sh\n", 0o755),
		file("lib/pkgconfig/jq.pc", "prefix=@@HOMEBREW_PREFIX@@\n", 0),
	}, Changed: []string{"lib/pkgconfig/jq.pc"}}
}

func exists(t *testing.T, prefix, rel string) bool {
	t.Helper()
	_, err := os.Lstat(filepath.Join(prefix, rel))
	return err == nil
}

func TestInstall(t *testing.T) {
	s := newStore(t)
	s.formula(jqBottle(), nil, nil)
	e, prefix := s.env()
	out, err := runCmd(t, e, "install", "-prefix", prefix, "jq")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"==> Fetching downloads for: jq\n", "==> Pouring jq 1.0\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin/jq")); got != "../Cellar/jq/1.0/bin/jq" {
		t.Errorf("bin/jq -> %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "Cellar/jq/1.0/lib/pkgconfig/jq.pc")); string(b) != "prefix="+prefix+"\n" {
		t.Errorf("pkgconfig file = %q", b)
	}
	kegs, err := cellar.Prefix(prefix).Kegs()
	if err != nil || len(kegs) != 1 || kegs[0].Receipt == nil || !kegs[0].Receipt.InstalledOnRequest {
		t.Errorf("kegs = %+v, %v", kegs, err)
	}

	// Installing it again changes nothing and says why.
	out, err = runCmd(t, e, "install", "-prefix", prefix, "jq")
	if err != nil || strings.Contains(out, "Pouring") {
		t.Errorf("second install: %q, %v", out, err)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "jq is already installed") {
		t.Errorf("stderr = %q", w)
	}
}

func TestInstallBringsDependenciesFirst(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "libx", Version: "2.0", Files: []installtest.File{file("lib/libx.a", "x", 0)}}, nil, nil)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}, Deps: []string{"libx"}}, []string{"libx"}, nil)
	e, prefix := s.env()
	out, err := runCmd(t, e, "install", "-prefix", prefix, "app")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Index(out, "Pouring libx") > strings.Index(out, "Pouring app") || !strings.Contains(out, "Pouring libx") {
		t.Errorf("order:\n%s", out)
	}
	kegs, _ := cellar.Prefix(prefix).Kegs()
	requested := map[string]bool{}
	for _, k := range kegs {
		requested[k.Name] = k.Receipt.InstalledOnRequest
	}
	if len(requested) != 2 || requested["libx"] || !requested["app"] {
		t.Errorf("on request = %v: only the named formula is", requested)
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "libx", Version: "2.0", Files: []installtest.File{file("lib/libx.a", "x", 0)}}, nil, nil)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}}, []string{"libx"}, nil)
	e, prefix := s.env()
	out, err := runCmd(t, e, "install", "--dry-run", "-prefix", prefix, "app")
	if err != nil {
		t.Fatal(err)
	}
	if want := "==> Would install 2 formulae:\nlibx 2.0\napp 1.0\n"; out != want {
		t.Errorf("output = %q, want %q", out, want)
	}
	entries, _ := os.ReadDir(prefix)
	if len(entries) != 0 {
		t.Errorf("a dry run wrote %v", entries)
	}
	if n := s.reg.ManifestRequests(); n != 0 {
		t.Errorf("a dry run asked the registry %d times", n)
	}
}

func TestInstallExplainsWhatItRefuses(t *testing.T) {
	s := newStore(t)
	s.formula(jqBottle(), nil, nil)
	s.formula(installtest.Bottle{Name: "pi", Version: "1.0"}, nil, map[string]any{"post_install_steps": []any{map[string]any{"type": "gtk_update_icon_cache"}}})
	s.formula(installtest.Bottle{Name: "old", Version: "1.0"}, nil, map[string]any{"disabled": true})
	e, prefix := s.env()
	_, err := runCmd(t, e, "install", "-prefix", prefix, "jq", "pi", "old")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"pi:", "gtk_update_icon_cache", "old:", "disabled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q: %v", want, err)
		}
	}
	if exists(t, prefix, "Cellar") {
		t.Error("something was installed although the plan was refused")
	}
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "nosuchformula"); err == nil {
		t.Error("an unknown formula should fail")
	}
	if _, err := runCmd(t, e, "install", "-prefix", prefix); err == nil {
		t.Error("install needs a formula")
	}
}

func TestInstallLinkConflictIsReportedAndFails(t *testing.T) {
	s := newStore(t)
	s.formula(jqBottle(), nil, nil)
	e, prefix := s.env()
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "bin/jq"), []byte("mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runCmd(t, e, "install", "-prefix", prefix, "jq")
	if err == nil || !strings.Contains(err.Error(), "not linked") {
		t.Errorf("error = %v, want one that says the keg was not linked", err)
	}
	if !exists(t, prefix, "Cellar/jq/1.0/bin/jq") {
		t.Error("the keg should be poured")
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "bin/jq") {
		t.Errorf("stderr does not name the conflict: %q", w)
	}
}

func TestInstallPrintsCaveatsAndKegOnlyNotice(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "ko", Version: "1.0", Files: []installtest.File{file("lib/libko.a", "k", 0)}}, nil,
		map[string]any{"keg_only": true, "caveats": "Look in $HOMEBREW_PREFIX/etc/ko.\n"})
	e, prefix := s.env()
	out, err := runCmd(t, e, "install", "-prefix", prefix, "ko")
	if err != nil {
		t.Fatal(err)
	}
	want := "==> Caveats\n==> ko\nLook in " + prefix + "/etc/ko.\n\nko is keg-only, which means it was not symlinked into " + prefix + "\n"
	if !strings.Contains(out, want) {
		t.Errorf("caveats should hold the formula's text, a blank line, then the keg-only notice:\n%s", out)
	}
	if exists(t, prefix, "lib/libko.a") || !exists(t, prefix, "opt/ko") {
		t.Error("a keg-only formula should only have its opt link")
	}
}

func TestInstallWritesNothingOutsideThePrefixAndTheCaches(t *testing.T) {
	s := newStore(t)
	s.formula(jqBottle(), nil, nil)
	e, prefix := s.env()
	cache := e.client.CacheDir
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "jq"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, cache, "downloads") {
		t.Error("bottles should be cached next to the formula list")
	}
}

func TestInstallRunsPostInstallAndReportsFailures(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "ok", Version: "1.0", Files: []installtest.File{file("bin/ok", "o", 0o755)}}, nil,
		map[string]any{"post_install_steps": []any{map[string]any{"type": "mkdir_p", "path": map[string]any{"base": "var", "path": "lib/ok"}}}})
	s.formula(installtest.Bottle{Name: "bad", Version: "1.0", Files: []installtest.File{file("bin/bad", "b", 0o755)}}, nil,
		map[string]any{"post_install_steps": []any{map[string]any{"type": "run", "command": map[string]any{"path": "false"}}}})
	e, prefix := s.env()
	_, err := runCmd(t, e, "install", "-prefix", prefix, "ok", "bad")
	if err == nil || !strings.Contains(err.Error(), "post_install") {
		t.Errorf("error = %v, want one about post_install", err)
	}
	if !exists(t, prefix, "var/lib/ok") || !exists(t, prefix, "Cellar/bad/1.0") {
		t.Error("the steps that can run should run, and a failing formula stays installed")
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "bad") || !strings.Contains(w, "post_install") {
		t.Errorf("stderr = %q", w)
	}
}

func TestSBOMCreatorNamesBrwWithItsVersionOrCommit(t *testing.T) {
	const base = "Tool: https://github.com/jigsmiths/brw"
	for _, tt := range []struct{ version, revision, want string }{
		{"v0.3.1", "abcdef1234567", base + "@v0.3.1"},
		{"(devel)", "abcdef1234567", base + "@abcdef1"},
		{"", "abcdef1234567", base + "@abcdef1"},
		{"(devel)", "abc", base},
		{"", "", base},
	} {
		if got := sbomCreatorFor(tt.version, tt.revision); got != tt.want {
			t.Errorf("sbomCreatorFor(%q, %q) = %q, want %q", tt.version, tt.revision, got, tt.want)
		}
	}
	if got := sbomCreator(); !strings.HasPrefix(got, base) {
		t.Errorf("sbomCreator() = %q", got)
	}
}

func TestInstallWritesBrwIntoTheSBOM(t *testing.T) {
	s := newStore(t)
	sbom := `{"creationInfo":{"created":"2026-10-01T00:00:00Z","creators":["Tool: https://github.com/Homebrew/brew"]},"packages":[]}`
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755), file("sbom.spdx.json", sbom, 0)}}, nil, nil)
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(prefix, "Cellar/app/1.0/sbom.spdx.json"))
	if err != nil || !strings.Contains(string(b), "Tool: https://github.com/jigsmiths/brw") || strings.Contains(string(b), "2026-10-01") {
		t.Errorf("the SBOM = %s, %v", b, err)
	}
}
