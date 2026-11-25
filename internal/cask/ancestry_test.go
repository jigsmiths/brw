package cask

import (
	"context"
	"errors"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
				return []byte(parents[args[3]] + "\n"), nil
			},
			"/usr/bin/lsappinfo": func(args []string) ([]byte, error) {
				switch args[0] {
				case "find":
					if args[1] == "pid=400" {
						return []byte("ASN:0x0-0x1-\"Term\":\n"), nil
					}
					return []byte("\n"), nil
				case "info":
					return []byte("[ NULL ]  ASN:0x0-0x1: \n    bundleID=\"com.term\"\n"), nil
				}
				return nil, errors.New("unexpected")
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

func TestLSAppInfoParsing(t *testing.T) {
	list := "  1) \"loginwindow\" ASN:0x0-0x2002: \n    bundleID=\"com.apple.loginwindow\"\n    pid = 442 type=\"UIElement\"\n" +
		" 2) \"daemon\" ASN:0x0-0x3: \n    bundleID=[ NULL ] \n 3) \"Finder\" ASN:0x0-0x4: \n    bundleID=\"com.apple.finder\"\n"
	if got := lsappinfoBundleIDs(list); !slices.Equal(got, []string{"com.apple.loginwindow", "com.apple.finder"}) {
		t.Errorf("bundle IDs = %q", got)
	}
	if got := lsappinfoBundleIDs("    bundleID=[ NULL ] \n"); len(got) != 0 {
		t.Errorf("bundle IDs = %q", got)
	}
}

func TestUninstallQuitRetriesWithoutPausing(t *testing.T) {
	ti, f := quitting(t, `{"quit":"com.example.app"}`)
	var slept []time.Duration
	ti.in.Sleep = func(d time.Duration) { slept = append(slept, d) }
	attempts := 0
	f.handle = map[string]func([]string) ([]byte, error){
		"osascript": func(args []string) ([]byte, error) {
			if strings.Contains(args[3], "app.quit()") {
				attempts++
				if attempts <= 3 {
					return nil, errors.New("not yet")
				}
				return nil, nil
			}
			if attempts > 3 {
				return nil, errors.New("not running")
			}
			return nil, nil
		},
	}
	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if attempts != 4 || len(slept) != 0 {
		t.Errorf("attempts = %d, sleeps = %v", attempts, slept)
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
