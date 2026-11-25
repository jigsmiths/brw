package cask

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func quitting(t *testing.T, directive string) (*testInstall, *fakeExec) {
	t.Helper()
	ti, f := installed(t, `[{"app":["App.app"]}]`)
	receipt := ti.prefix + "/Caskroom/app/.metadata/INSTALL_RECEIPT.json"
	if err := os.WriteFile(receipt, []byte(`{"uninstall_artifacts":[{"uninstall":[`+directive+`]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return ti, f
}

func TestUninstallLeavesAnyAppBrwRunsInside(t *testing.T) {
	for _, directive := range []string{`{"quit":"com.term"}`, `{"signal":["TERM","com.term"]}`} {
		ti, f := quitting(t, directive)
		f.running["com.term"] = 1
		parents := map[string]string{strconv.Itoa(os.Getpid()): "500", "500": "400", "400": "500"}
		f.handle = map[string]func([]string) ([]byte, error){
			"/bin/ps": func(args []string) ([]byte, error) {
				if args[1] == "comm=" {
					if args[3] == "400" {
						return []byte("/Applications/Term.app/Contents/Frameworks/Helper.app/Contents/MacOS/Helper\n"), nil
					}
					return []byte("/usr/bin/login\n"), nil
				}
				return []byte(parents[args[3]] + "\n"), nil
			},
			"/usr/bin/plutil": func(args []string) ([]byte, error) {
				if args[len(args)-1] == "/Applications/Term.app/Contents/Frameworks/Helper.app/Contents/Info.plist" {
					return []byte("com.term\n"), nil
				}
				return nil, errors.New("no plist")
			},
			"/bin/launchctl": func([]string) ([]byte, error) {
				return []byte("PID\tStatus\tLabel\n9\t0\tcom.term\n"), nil
			},
		}
		if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(ti.warn.String(), "Skipping ") || !strings.Contains(ti.warn.String(), " application 'com.term' as `brw` is running inside it.") {
			t.Errorf("%s: warnings = %q", directive, ti.warn.String())
		}
		if strings.Contains(ti.out.String(), "Quitting") || strings.Contains(ti.out.String(), "Signalling") {
			t.Errorf("%s: output = %q", directive, ti.out.String())
		}
		for _, want := range []string{"/bin/ps -o ppid= -p 400", "/bin/ps -o ppid= -p 500"} {
			if !slices.Contains(f.calls, want) {
				t.Errorf("%s: calls = %q, missing %q", directive, f.calls, want)
			}
		}
	}
}

func TestAppBundleOf(t *testing.T) {
	for comm, want := range map[string]string{
		"/Applications/Foo.app/Contents/MacOS/Foo":          "/Applications/Foo.app",
		"/Applications/My App.app/Contents/MacOS/My App":    "/Applications/My App.app",
		"/A.app/Contents/Frameworks/H.app/Contents/MacOS/H": "/A.app/Contents/Frameworks/H.app",
		"/usr/bin/zsh": "",
		"/Applications/Foo.app/Contents/Resources/tool": "",
		"zsh": "",
	} {
		if got := appBundleOf(comm); got != want {
			t.Errorf("appBundleOf(%q) = %q, want %q", comm, got, want)
		}
	}
}

func TestUninstallQuitExpandsWildcards(t *testing.T) {
	ti, f := quitting(t, `{"quit":"com.EXAMPLE.*"}`)
	f.runningIDs = "com.example.app\norg.other.com.example.app\ncom.other\n"
	f.running["com.example.app"] = 1
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if got := ti.out.String(); !strings.Contains(got, "==> Quitting application 'com.example.app'...") || strings.Contains(got, "org.other") || strings.Contains(got, "com.other") {
		t.Errorf("out = %q", got)
	}
}

func TestUninstallQuitWildcardNeedsAGUI(t *testing.T) {
	ti, f := quitting(t, `{"quit":"com.example.*"}`)
	f.gui = false
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: Not logged into a GUI; skipping applications matching 'com.example.*'.\n"; ti.warn.String() != want {
		t.Errorf("warn = %q", ti.warn.String())
	}
}

func TestUninstallQuitNeedsAGUI(t *testing.T) {
	ti, f := quitting(t, `{"quit":"com.example.app"}`)
	f.gui = false
	f.running["com.example.app"] = 1
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if want := "Warning: Not logged into a GUI; skipping quitting application ID 'com.example.app'.\n"; ti.warn.String() != want {
		t.Errorf("warn = %q", ti.warn.String())
	}
	if strings.Contains(ti.out.String(), "Quitting") {
		t.Errorf("out = %q", ti.out.String())
	}
}

func TestEmbeddedRmdirScriptIsBrews(t *testing.T) {
	brews, err := os.ReadFile("/opt/homebrew/Library/Homebrew/cask/utils/rmdir.sh")
	if err != nil {
		t.Skip("no Homebrew checkout to compare with")
	}
	if rmdirScript != string(brews) {
		t.Error("the embedded rmdir.sh differs from brew's")
	}
}
