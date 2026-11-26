package cask

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/sudo"
)

func TestOnlyBrewsSystemCommandsEchoUnderVerboseAndDebug(t *testing.T) {
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
		{"diff", func(in *Installer) { _, _ = in.sameApp(ctx, "/a/A.app", "/b/A.app") }, "/usr/bin/env /usr/bin/diff --recursive --brief /a/A.app /b/A.app\n"},
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
			ti.in.warnedSandbox = true
			ti.in.Exec = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
			tc.run(ti.in)
			got := ti.warn.String()
			if tc.want == "" && got != "" || tc.want != "" && !strings.Contains(got, tc.want) {
				t.Errorf("trace = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTraceGates(t *testing.T) {
	for _, tc := range []struct {
		name           string
		verbose, debug bool
		env            map[string]string
		nilWarn        bool
		want           bool
	}{
		{"neither", false, false, nil, false, false},
		{"verbose flag alone", true, false, nil, false, false},
		{"debug flag alone", false, true, nil, false, false},
		{"HOMEBREW_VERBOSE alone", false, false, map[string]string{"HOMEBREW_VERBOSE": "1"}, false, false},
		{"HOMEBREW_VERBOSE and the debug flag", false, true, map[string]string{"HOMEBREW_VERBOSE": "1"}, false, true},
		{"HOMEBREW_VERBOSE and HOMEBREW_DEBUG", false, false, map[string]string{"HOMEBREW_VERBOSE": "1", "HOMEBREW_DEBUG": "1"}, false, true},
		{"verbose flag and HOMEBREW_DEBUG", true, false, map[string]string{"HOMEBREW_DEBUG": "1"}, false, true},
		{"both flags without a warning writer", true, true, nil, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOMEBREW_DEBUG", tc.env["HOMEBREW_DEBUG"])
			t.Setenv("HOMEBREW_VERBOSE", tc.env["HOMEBREW_VERBOSE"])
			ti := newTestInstall(t, appTarball)
			ti.in.Verbose, ti.in.Debug = tc.verbose, tc.debug
			ti.in.Exec = func(context.Context, string, ...string) ([]byte, error) { return nil, nil }
			if tc.nilWarn {
				ti.in.Warn = nil
			}
			_, _ = ti.in.system(context.Background(), "/bin/chmod", "u+rwx", "/x")
			if got := ti.warn.Len() > 0; got != tc.want {
				t.Errorf("echoed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestBundleVersionRunsPlutilLikeBrew(t *testing.T) {
	t.Setenv("HOMEBREW_DEBUG", "")
	t.Setenv("HOMEBREW_VERBOSE", "")
	plist := filepath.Join(t.TempDir(), "Info.plist")
	if err := os.WriteFile(plist, []byte(infoPlist("1.0", "100")), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, traced := range []bool{false, true} {
		ti := newTestInstall(t, appTarball)
		ti.in.Verbose, ti.in.Debug = traced, traced
		f := &fakeRun{handle: func(sudo.Command) (string, error) { return infoPlist("1.0", "100"), nil }}
		ti.in.Run = f.run
		short, version, ok, err := ti.in.bundleVersion(context.Background(), plist)
		if err != nil || !ok || short != "1.0" || version != "100" {
			t.Fatalf("bundleVersion = %q %q %v %v", short, version, ok, err)
		}
		c := f.calls[0]
		if c.Exe != "/usr/bin/plutil" || c.Privilege != sudo.User || !c.MustSucceed || c.Trace != nil != traced ||
			!slices.Equal(c.Args, []string{"-convert", "xml1", "-o", "-", plist}) {
			t.Errorf("traced=%v: run = %v", traced, c)
		}
	}
}

func TestInstallAdoptRaisesWhenPlutilFails(t *testing.T) {
	ti := versioned(t)
	existingApp(t, ti, "not a plist")
	err := ti.in.Install(context.Background(), ti.cask(t, appArtifacts))
	if err == nil || !strings.HasPrefix(err.Error(), "app: Failure while executing; `/usr/bin/env /usr/bin/plutil -convert xml1 -o - ") ||
		!strings.Contains(err.Error(), " exited with 1.") {
		t.Errorf("err = %v", err)
	}
	if exists(filepath.Join(ti.prefix, "Caskroom/app")) {
		t.Error("the failed install should be cleaned up")
	}
}
