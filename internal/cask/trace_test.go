package cask

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brew echoes the commands it runs through SystemCommand, but not the ones it reads with popen_read,
// FFI or Kernel#system, so each helper is pinned to the way brew runs it.
func TestOnlyBrewsSystemCommandsEchoUnderVerboseAndDebug(t *testing.T) {
	plist := filepath.Join(t.TempDir(), "Info.plist")
	if err := os.WriteFile(plist, []byte(infoPlist("1.0", "100")), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(in *Installer)
		want string
	}{
		{"who", func(in *Installer) { in.gui(ctx) }, "/usr/bin/env who\n"},
		{"osascript running", func(in *Installer) { in.running(ctx, "com.example") }, "/usr/bin/env osascript -l JavaScript -e "},
		{"launchctl list for wildcards", func(in *Installer) { in.runningServices(ctx, "com.example.*") }, "/usr/bin/env /bin/launchctl list\n"},
		{"launchctl list for processes", func(in *Installer) { _, _ = in.runningProcesses(ctx, "com.example") }, "/usr/bin/env /bin/launchctl list\n"},
		{"diff", func(in *Installer) { in.sameApp(ctx, "/a/A.app", "/b/A.app") }, "/usr/bin/env /usr/bin/diff --recursive --brief /a/A.app /b/A.app\n"},
		{"plutil", func(in *Installer) { in.bundleVersion(ctx, plist) }, "/usr/bin/env /usr/bin/plutil -convert xml1 -o - " + plist + "\n"},
		{"rm retry", func(in *Installer) { _ = in.sudoIfNeeded(ctx, "/bin/rm", "-f", "--", "/x") }, "/usr/bin/env /bin/rm -f -- /x\n"},
		{"ps", func(in *Installer) { in.parentPID(ctx, 1); in.ownedByMe(ctx, 1) }, ""},
		{"lsappinfo", func(in *Installer) { in.bundleIDForPID(ctx, 1) }, ""},
		{"trash", func(in *Installer) { in.trashItems(ctx, []string{"/x"}) }, ""},
		{"codesign", func(in *Installer) { in.signingIdentity(ctx, "/a/A.app") }, ""},
		{"open", func(in *Installer) { in.reopen = []string{"com.example"}; in.reopenApps(ctx, nil) }, ""},
		{"generated completions", func(in *Installer) { _, _ = in.sandboxed(ctx, []string{"/usr/bin/true"}) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOMEBREW_DEBUG", "")
			t.Setenv("HOMEBREW_VERBOSE", "")
			ti := newTestInstall(t, appTarball)
			ti.in.Verbose, ti.in.Debug = true, true
			ti.in.Exec = func(_ context.Context, name string, args ...string) ([]byte, error) {
				if name == "/usr/bin/plutil" {
					return []byte(infoPlist("1.0", "100")), nil
				}
				return nil, nil
			}
			tc.run(ti.in)
			got := ti.warn.String()
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("trace = %q, want %q", got, tc.want)
			}
		})
	}
}
