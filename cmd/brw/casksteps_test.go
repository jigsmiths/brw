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
	"path/filepath"
	"testing"

	"github.com/jigsmiths/brw/internal/fsx"
)

const permissionSteps = `{"postflight_steps":[{"steps":[{"paths":[{"base":"staged_path","path":"ngrok"}],"permissions":"0755","type":"set_permissions"}]}]}`

func (s *store) stepsCask(token string) {
	s.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	_ = tw.WriteHeader(&tar.Header{Name: "ngrok", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	_, _ = tw.Write([]byte("bin"))
	_ = tw.Close()
	_ = gz.Close()
	body := buf.Bytes()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(body) }))
	s.t.Cleanup(ts.Close)
	sum := sha256.Sum256(body)
	s.casks = append(s.casks, map[string]any{
		"token": token, "full_token": token, "tap": "homebrew/cask", "name": []string{"Ngrok"}, "version": "1.0",
		"url": ts.URL + "/ngrok-1.0.tar.gz", "sha256": hex.EncodeToString(sum[:]),
		"artifacts":  []json.RawMessage{json.RawMessage(`{"binary":["ngrok"]}`), json.RawMessage(permissionSteps)},
		"depends_on": map[string]any{},
	})
}

func TestCaskInstallerSetsWhatStepsRead(t *testing.T) {
	s := newStore(t)
	e, prefix := s.env()
	err := withRoot(prefix, "", func(root *fsx.Root) error {
		in := e.caskInstaller(root, prefix, filepath.Join(prefix, "Caskroom/x"), t.TempDir(), t.TempDir(), tracing{})
		for name, unset := range map[string]bool{
			"Root": in.Root == nil, "Where.Prefix": in.Where.Prefix == "", "Where.Home": in.Where.Home == "",
			"Where.AppDir": in.Where.AppDir == "", "Host.OS": in.Host.OS == "", "Sandbox": in.Sandbox == 0,
			"CacheDir": in.CacheDir == "", "Warn": in.Warn == nil,
		} {
			if unset {
				t.Errorf("%s is unset", name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestUninstallCaskWithFlightSteps(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	s := newStore(t)
	s.stepsCask("ngrok")
	e, prefix := s.env()
	apps := t.TempDir()
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "--cask", "ngrok"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	if out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--cask", "ngrok"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if exists(t, prefix, "Caskroom/ngrok") {
		t.Error("Caskroom/ngrok is still there")
	}
}
