package cask

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type sudoLink struct {
	ti     *testInstall
	f      *fakeExec
	dir    string
	source string
}

func (l sudoLink) target() string { return filepath.Join(l.dir, "kubectl") }

func (l sudoLink) artifacts(sudoValue string) string {
	return fmt.Sprintf(`[{"app":["App.app"]},{"postflight_steps":[{"steps":[{"type":"symlink","force":true,"uninstall":true,
		"sudo":%s,"source":{"path":"{{appdir}}/App.app/Contents/MacOS/app"},"target":{"path":%q},
		"guards":[{"path":%q,"condition":"unless_exists","id":"1"}]}]}]}]`, sudoValue, l.target(), l.target())
}

func newSudoLink(t *testing.T) sudoLink {
	t.Helper()
	ti := newTestInstall(t, appTarball)
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}, gui: true}
	ti.in.Exec, ti.in.Run, ti.in.FindService = f.exec, f.run, f.findService
	return sudoLink{ti: ti, f: f, dir: t.TempDir(), source: filepath.Join(ti.appDir, "App.app/Contents/MacOS/app")}
}

func (l sudoLink) install(t *testing.T, sudoValue string) error {
	t.Helper()
	return l.ti.in.Install(context.Background(), l.ti.cask(t, l.artifacts(sudoValue)))
}

func TestPostflightSudoSymlinkRunsLnThroughSudoAndUninstallRemovesIt(t *testing.T) {
	l := newSudoLink(t)
	if err := l.install(t, `true`); err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/sudo -E -- /bin/ln -s -f " + l.source + " " + l.target()}
	if got := l.f.ran("/usr/bin/sudo"); !slices.Equal(got, want) {
		t.Errorf("sudo calls = %q, want %q", got, want)
	}
	if got := readLink(t, l.target()); got != l.source {
		t.Errorf("link -> %s", got)
	}
	if err := l.ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if exists(l.target()) {
		t.Error("uninstall left the link")
	}
}

func TestPostflightIfNeededSymlinkUsesSudoInALockedDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	l := newSudoLink(t)
	lock(t, l.dir)
	if err := l.install(t, `"if_needed"`); err != nil {
		t.Fatal(err)
	}
	want := []string{"/usr/bin/sudo -E -- /bin/ln -s -f " + l.source + " " + l.target()}
	if got := l.f.ran("/usr/bin/sudo"); !slices.Equal(got, want) {
		t.Errorf("sudo calls = %q, want %q", got, want)
	}
	if err := l.ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if got := l.f.ran("/usr/bin/sudo"); !slices.Contains(got, "/usr/bin/sudo -E -- /bin/rm -f -- "+l.target()) {
		t.Errorf("uninstall did not remove the link with sudo: %q", got)
	}
	if exists(l.target()) {
		t.Error("uninstall left the link")
	}
}

func TestPostflightIfNeededSymlinkInAWritableDirectoryNeedsNoSudo(t *testing.T) {
	l := newSudoLink(t)
	if err := l.install(t, `"if_needed"`); err != nil {
		t.Fatal(err)
	}
	if got := l.f.ran("/usr/bin/sudo"); len(got) != 0 {
		t.Errorf("sudo ran: %q", got)
	}
	if got := readLink(t, l.target()); got != l.source {
		t.Errorf("link -> %s", got)
	}
}

func TestPostflightSymlinkGuardSkipsAnExistingTarget(t *testing.T) {
	l := newSudoLink(t)
	if err := os.WriteFile(l.target(), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.install(t, `true`); err != nil {
		t.Fatal(err)
	}
	if got := l.f.ran("/usr/bin/sudo"); len(got) != 0 {
		t.Errorf("sudo ran: %q", got)
	}
	if b, _ := os.ReadFile(l.target()); string(b) != "mine" {
		t.Errorf("the existing file changed: %q", b)
	}
}

func TestPostflightSudoSymlinkRefusesWhenSudoIsDisabled(t *testing.T) {
	l := newSudoLink(t)
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	err := l.install(t, `true`)
	want := "app: The postflight_steps artifact requires sudo, but HOMEBREW_NO_SUDO is set."
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %s", err, want)
	}
	if got := l.f.ran("/usr/bin/sudo"); len(got) != 0 {
		t.Errorf("sudo ran: %q", got)
	}
	if _, err := os.Stat(filepath.Join(l.ti.prefix, "Caskroom/app")); err == nil {
		t.Error("a refused cask was installed")
	}
}

func TestPostflightIfNeededSymlinkFailsWithoutSudoInALockedDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	l := newSudoLink(t)
	l.ti.in.Run, l.ti.in.Exec = nil, nil
	lock(t, l.dir)
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	err := l.install(t, `"if_needed"`)
	if err == nil || !strings.Contains(err.Error(), "sudo is disabled by HOMEBREW_NO_SUDO.") {
		t.Errorf("err = %v", err)
	}
	if exists(l.target()) {
		t.Error("the link was made without sudo")
	}
}
