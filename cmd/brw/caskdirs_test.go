package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRecordsDirFlagsAndUninstallUsesThemWithoutFlags(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	t.Setenv("HOMEBREW_CASK_OPTS", "")
	s := newStore(t)
	s.appCask("app")
	e, prefix := s.env()
	apps := filepath.Join(t.TempDir(), "Apps")
	layouts := filepath.Join(t.TempDir(), "Layouts")
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "--keyboard-layoutdir="+layouts, "--cask", "app"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	config, err := os.ReadFile(filepath.Join(prefix, "Caskroom/app/.metadata/config.json"))
	want := `"explicit":{"appdir":"` + apps + `","keyboard_layoutdir":"` + layouts + `"}}`
	if err != nil || !strings.Contains(string(config), want) {
		t.Errorf("config.json = %s, %v; want it to contain %s", config, err, want)
	}
	if out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--cask", "app"); err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	if exists(t, apps, "App.app") {
		t.Error("uninstall should remove the app from the recorded appdir")
	}
}

func TestCaskDirEnvironmentIsRecordedAsEnv(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	pickers := filepath.Join(t.TempDir(), "Pickers")
	t.Setenv("HOMEBREW_CASK_OPTS", "--colorpickerdir="+pickers)
	s := newStore(t)
	s.appCask("app")
	e, prefix := s.env()
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "--cask", "app"); err != nil {
		t.Fatalf("install: %v\n%s", err, out)
	}
	config, _ := os.ReadFile(filepath.Join(prefix, "Caskroom/app/.metadata/config.json"))
	if !strings.Contains(string(config), `"env":{"colorpickerdir":"`+pickers+`"}`) {
		t.Errorf("config.json = %s", config)
	}
}

func TestInstallRejectsDirFlagsWithFormula(t *testing.T) {
	s := newStore(t)
	e, prefix := s.env()
	_, err := runCmd(t, e, "install", "-prefix", prefix, "--formula", "--fontdir", "/x", "jq")
	if want := "Options --formula and --fontdir are mutually exclusive."; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
}
