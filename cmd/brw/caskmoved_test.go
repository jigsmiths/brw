package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func (s *store) fontCask(token string) {
	s.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "Font.ttf", Mode: 0o644, Size: 4, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("font"))
	_ = tw.Close()
	_ = gz.Close()
	body := buf.Bytes()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	s.t.Cleanup(ts.Close)
	sum := sha256.Sum256(body)
	s.casks = append(s.casks, map[string]any{
		"token": token, "full_token": token, "tap": "homebrew/cask", "name": []string{"Font"}, "version": "1.0",
		"url": ts.URL + "/Font-1.0.tar.gz", "sha256": hex.EncodeToString(sum[:]),
		"artifacts":  []json.RawMessage{json.RawMessage(`{"font":["Font.ttf"]}`)},
		"depends_on": map[string]any{},
	})
}

func TestInstallFontIntoFontdirThenUninstallWithoutFlags(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	t.Setenv("HOMEBREW_CASK_OPTS", "")
	s := newStore(t)
	s.fontCask("myfont")
	e, prefix := s.env()
	fonts := filepath.Join(t.TempDir(), "Fonts")
	target := filepath.Join(fonts, "Font.ttf")
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--fontdir", fonts, "--cask", "myfont"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if b, err := os.ReadFile(target); err != nil || string(b) != "font" {
		t.Fatalf("font = %q, %v", b, err)
	}
	config, err := os.ReadFile(filepath.Join(prefix, "Caskroom/myfont/.metadata/config.json"))
	if err != nil || !strings.Contains(string(config), `"explicit":{"fontdir":"`+fonts+`"}`) {
		t.Errorf("config.json = %s, %v", config, err)
	}
	if out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--cask", "myfont"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if exists(t, fonts, "Font.ttf") || exists(t, prefix, "Caskroom/myfont") {
		t.Error("uninstall should remove the font from the recorded fontdir and the Caskroom folder")
	}
	if err := os.WriteFile(target, []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = runCmd(t, e, "install", "-prefix", prefix, "--fontdir", fonts, "--cask", "myfont")
	if err == nil {
		t.Fatal("installing over an existing font should fail")
	}
	if got := e.err.(*bytes.Buffer).String(); !strings.Contains(got, "It seems there is already a Font at '"+target+"'.") {
		t.Errorf("stderr = %q", got)
	}
}

func TestCaskDirEnvironmentIsRecordedAndUsed(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	fonts := filepath.Join(t.TempDir(), "EnvFonts")
	t.Setenv("HOMEBREW_CASK_OPTS", "--fontdir="+fonts)
	s := newStore(t)
	s.fontCask("myfont")
	e, prefix := s.env()
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--cask", "myfont"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if !exists(t, fonts, "Font.ttf") {
		t.Error("HOMEBREW_CASK_OPTS --fontdir was not honored")
	}
	config, _ := os.ReadFile(filepath.Join(prefix, "Caskroom/myfont/.metadata/config.json"))
	if !strings.Contains(string(config), `"env":{"fontdir":"`+fonts+`"},"explicit":{}`) {
		t.Errorf("config.json = %s", config)
	}
}
