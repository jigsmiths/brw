package cask

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const intlCacheClear = "/usr/bin/sudo -u root -E -- /bin/rm -f -- /System/Library/Caches/com.apple.IntlDataCache.le*"

type layoutInstall struct {
	sudoLink
	layouts string
}

func newLayoutInstall(t *testing.T) layoutInstall {
	t.Helper()
	l := newSudoLink(t)
	layouts := t.TempDir()
	l.ti.in.Where.Dirs[DirKeyboard] = layouts
	l.f.handle = map[string]func([]string) ([]byte, error){"/usr/bin/sudo": func([]string) ([]byte, error) { return nil, nil }}
	return layoutInstall{sudoLink: l, layouts: layouts}
}

func (l layoutInstall) installLayout(t *testing.T) error {
	t.Helper()
	return l.ti.in.Install(context.Background(), l.ti.cask(t, `[{"keyboard_layout":["App.app"]}]`))
}

func TestKeyboardLayoutIsMovedLinkedAndTheIntlCacheCleared(t *testing.T) {
	l := newLayoutInstall(t)
	t.Setenv("HOMEBREW_NO_SUDO", "")
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	if err := l.installLayout(t); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(l.layouts, "App.app")
	staged := filepath.Join(l.ti.prefix, "Caskroom/app/1.0/App.app")
	if _, err := os.Stat(filepath.Join(target, "Contents/Info.plist")); err != nil {
		t.Errorf("the layout was not moved: %v", err)
	}
	if got := readLink(t, staged); got != target {
		t.Errorf("staged link -> %s, want %s", got, target)
	}
	if got := l.f.ran("/usr/bin/sudo"); !slices.Equal(got, []string{intlCacheClear}) {
		t.Errorf("sudo calls = %q, want only %q", got, intlCacheClear)
	}
	if !strings.Contains(l.ti.out.String(), "==> Moving Keyboard Layout 'App.app' to '"+target+"'") {
		t.Errorf("output = %q", l.ti.out.String())
	}
	receipt, err := os.ReadFile(filepath.Join(l.ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil || !strings.Contains(string(receipt), `"keyboard_layout"`) {
		t.Errorf("receipt = %s %v", receipt, err)
	}

	l.f.calls = nil
	if err := l.ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if exists(target) {
		t.Error("uninstall left the layout in place")
	}
	if fi, err := os.Lstat(staged); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Error("uninstall left the staged link instead of moving the layout back")
	}
	if got := l.f.ran("/usr/bin/sudo"); !slices.Equal(got, []string{intlCacheClear}) {
		t.Errorf("sudo calls on uninstall = %q, want %q", got, intlCacheClear)
	}
}

func TestKeyboardLayoutDirOptionIsRecordedAsExplicit(t *testing.T) {
	l := newLayoutInstall(t)
	t.Setenv("HOMEBREW_NO_SUDO", "")
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	l.ti.in.Config.Explicit = map[DirKey]string{DirKeyboard: l.layouts}
	if err := l.installLayout(t); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(l.ti.prefix, "Caskroom/app/.metadata/config.json"))
	if err != nil || !strings.Contains(string(b), `"keyboard_layoutdir":"`+l.layouts+`"}}`) {
		t.Errorf("config.json = %s, %v", b, err)
	}
}

func TestKeyboardLayoutRequiresSudoUpFront(t *testing.T) {
	l := newLayoutInstall(t)
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	err := l.installLayout(t)
	want := "app: The keyboard_layout artifact requires sudo, but HOMEBREW_NO_SUDO is set."
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
	if _, err := os.Stat(filepath.Join(l.ti.prefix, "Caskroom/app")); err == nil {
		t.Error("a refused cask was installed")
	}
}

func TestKeyboardLayoutRefusesAnOccupiedTarget(t *testing.T) {
	l := newLayoutInstall(t)
	t.Setenv("HOMEBREW_NO_SUDO", "")
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	if err := os.Mkdir(filepath.Join(l.layouts, "App.app"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := l.installLayout(t)
	want := "It seems there is already a Keyboard Layout at '" + filepath.Join(l.layouts, "App.app") + "'."
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %s", err, want)
	}
}

func TestSudoArtifactNamesKeyboardLayoutAfterPkgAndBeforePostflight(t *testing.T) {
	p := installPlan{moved: []move{{kind: "keyboard_layout"}}, postflight: raws(t, `[{"type":"run","sudo":true}]`)}
	if got := p.sudoArtifact(); got != "keyboard_layout" {
		t.Errorf("sudoArtifact = %q, want keyboard_layout", got)
	}
	p.pkgs = []pkgInstall{{}}
	if got := p.sudoArtifact(); got != "pkg" {
		t.Errorf("sudoArtifact = %q, want pkg", got)
	}
}
