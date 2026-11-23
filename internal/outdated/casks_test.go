package outdated

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
)

func caskIndex(t *testing.T, extra ...api.Cask) *api.CaskIndex {
	t.Helper()
	b, err := os.ReadFile("../api/testdata/casks.json")
	if err != nil {
		t.Fatal(err)
	}
	casks, err := api.ParseCasks(b)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewCaskIndex(append(casks, extra...))
}

func bundles(apps map[string][2]string) func(string) (string, string, bool) {
	return func(app string) (string, string, bool) {
		v, ok := apps[app]
		return v[0], v[1], ok
	}
}

func options(apps map[string][2]string) CaskOptions {
	return CaskOptions{
		Tag:    "arm64_golden_gate",
		AppDir: func(string) string { return "/Applications" },
		Bundle: bundles(apps),
		Home:   "/Users/me",
	}
}

func caskLines(rs []Result) []string {
	var out []string
	for _, r := range rs {
		out = append(out, r.Name+" "+r.InstalledVersions[0]+" "+r.CurrentVersion)
	}
	return out
}

func TestCheckCasks(t *testing.T) {
	disabled := api.Cask{Token: "gone", Tap: "homebrew/cask", Version: "2.0", Disabled: true}
	idx := caskIndex(t, disabled)
	installed := []caskroom.Cask{
		{Token: "1password-cli", Version: "2.40.0", Tap: "homebrew/cask"},
		{Token: "1password@nightly", Version: "latest"},
		{Token: "aptible", Version: "1.0.7"},                  // up to date
		{Token: "bridge", Version: "2024.0.0"},                // deprecated still counts
		{Token: "gone", Version: "1.0"},                       // disabled
		{Token: "google-cloud-sdk", Version: "575.0.0"},       // an old token
		{Token: "notinapi", Version: "1.0"},                   // from a tap brw cannot read
		{Token: "slack", Version: "4.50.0", Tap: "other/tap"}, // installed from another tap
		{Token: "4k-video-downloader+", Version: "26.4.0"},    // up to date
	}
	got, err := CheckCasks(idx, t.TempDir(), installed, options(nil))
	if err != nil {
		t.Fatal(err)
	}
	// gcloud-cli and bridge update themselves and have no app bundle to tell by.
	want := []string{"1password-cli 2.40.0 2.42.0"}
	if !slices.Equal(caskLines(got), want) {
		t.Errorf("CheckCasks = %q, want %q", caskLines(got), want)
	}

	o := options(nil)
	o.Greedy = true
	got, err = CheckCasks(idx, t.TempDir(), installed, o)
	if err != nil {
		t.Fatal(err)
	}
	// Greedy: self-updating casks count, latest and disabled ones still do not.
	want = []string{"1password-cli 2.40.0 2.42.0", "bridge 2024.0.0 2025.0.0", "gcloud-cli 575.0.0 594.0.0"}
	if !slices.Equal(caskLines(got), want) {
		t.Errorf("greedy CheckCasks = %q, want %q", caskLines(got), want)
	}
}

func TestCheckCasksFromATap(t *testing.T) {
	idx := caskIndex(t)
	idx.AddTap([]api.Cask{
		{Token: "brw", Tap: "jigsmiths/tap", Version: "0.20.0"},
		{Token: "slack", Tap: "jigsmiths/tap", Version: "9.0"},
	})
	installed := []caskroom.Cask{
		{Token: "brw", Version: "0.19.0", Tap: "jigsmiths/tap"},
		{Token: "slack", Version: "4.50.0", Tap: "jigsmiths/tap"},
		{Token: "1password-cli", Version: "2.40.0"}, // no receipt: from the API
	}
	got, err := CheckCasks(idx, t.TempDir(), installed, options(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"brw 0.19.0 0.20.0", "slack 4.50.0 9.0", "1password-cli 2.40.0 2.42.0"}
	if !slices.Equal(caskLines(got), want) {
		t.Errorf("CheckCasks = %q, want %q", caskLines(got), want)
	}
}

func TestCheckCasksAutoUpdates(t *testing.T) {
	idx := caskIndex(t)
	installed := []caskroom.Cask{{Token: "slack", Version: "4.50.0"}}
	selfUpdated := map[string][2]string{"/Applications/Slack.app": {"4.53.292", "453000292"}}
	behind := map[string][2]string{"/Applications/Slack.app": {"4.50.0", "450000000"}}

	for _, tc := range []struct {
		name string
		apps map[string][2]string
		set  func(*CaskOptions)
		want bool
	}{
		{"the app updated itself", selfUpdated, nil, false},
		{"the app is behind", behind, nil, true},
		{"no app to read", nil, nil, false},
		{"behind but auto updates are left alone", behind, func(o *CaskOptions) { o.NoAutoUpdates = true }, false},
		{"--greedy", selfUpdated, func(o *CaskOptions) { o.Greedy = true }, true},
		{"--greedy-auto-updates", selfUpdated, func(o *CaskOptions) { o.GreedyAutoUpdates = true }, true},
		{"greedy for this cask", selfUpdated, func(o *CaskOptions) { o.GreedyTokens = []string{"slack"} }, true},
		{"greedy for another cask", selfUpdated, func(o *CaskOptions) { o.GreedyTokens = []string{"zed"} }, false},
		{"--greedy-latest only", selfUpdated, func(o *CaskOptions) { o.GreedyLatest = true }, false},
	} {
		o := options(tc.apps)
		if tc.set != nil {
			tc.set(&o)
		}
		got, err := CheckCasks(idx, t.TempDir(), installed, o)
		if err != nil {
			t.Fatal(err)
		}
		if (len(got) == 1) != tc.want {
			t.Errorf("%s: got %q", tc.name, caskLines(got))
		}
	}
}

func TestCheckCasksAppliesTheVariation(t *testing.T) {
	installed := []caskroom.Cask{{Token: "adobe-connect", Version: "2026.10.66,11"}}
	o := options(nil)
	o.Tag, o.Greedy = "golden_gate", true
	got, err := CheckCasks(caskIndex(t), t.TempDir(), installed, o)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].CurrentVersion != "2026.10.170,11" {
		t.Errorf("got %q", caskLines(got))
	}
}

func TestCheckCasksPinned(t *testing.T) {
	prefix := t.TempDir()
	pins := filepath.Join(prefix, "var/homebrew/pinned_casks")
	if err := os.MkdirAll(filepath.Join(prefix, "Caskroom/1password-cli/2.40.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(pins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(prefix, "Caskroom/1password-cli/2.40.0"), filepath.Join(pins, "1password-cli")); err != nil {
		t.Fatal(err)
	}
	got, err := CheckCasks(caskIndex(t), prefix, []caskroom.Cask{{Token: "1password-cli", Version: "2.40.0"}}, options(nil))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got[0].Pinned || got[0].PinnedVersion != "2.40.0" {
		t.Errorf("got %+v", got)
	}
}

func TestBundleOutdated(t *testing.T) {
	for _, tc := range []struct {
		name               string
		version, installed string
		short, long        string
		want               bool
	}{
		{"same short version", "4.53.292", "4.50.0", "4.53.292", "453000292", false},
		{"older short version", "4.53.292", "4.50.0", "4.50.0", "450000000", true},
		{"newer short version", "4.53.292", "4.50.0", "4.60.0", "", false},
		{"long version matches", "4.53.292", "4.50.0", "", "4.53.292", false},
		{"split across both fields", "2.61-2057", "2.60-2000", "2.61", "2057", false},
		{"placeholder versions", "1.2.3", "1.0.0", "0", "0.0", false},
		{"older build of a csv version", "1.2.3,456", "1.2.0,400", "", "450", true},
		{"same build of a csv version", "1.2.3,456", "1.2.0,400", "", "456", false},
		{"unlike short version that Homebrew installed", "4.1", "4", "4", "", true},
		{"unlike short version Homebrew did not install", "4.1", "3", "4", "", false},
	} {
		c := api.Cask{Token: "x", Version: tc.version, AutoUpdates: true, Artifacts: []json.RawMessage{json.RawMessage(`{"app": ["X.app"]}`)}}
		o := options(map[string][2]string{"/Applications/X.app": {tc.short, tc.long}})
		if got := bundleOutdated(c, caskroom.Cask{Token: "x", Version: tc.installed}, o); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAppTarget(t *testing.T) {
	for _, tc := range []struct {
		artifacts []string
		want      string
	}{
		{[]string{`{"app": ["Slack.app"]}`}, "/Apps/Slack.app"},
		{[]string{`{"uninstall": [{"quit": "x"}]}`, `{"app": ["sub/dir/Foo.app"]}`, `{"zap": []}`}, "/Apps/Foo.app"},
		{[]string{`{"app": ["Foo.app", {"target": "Bar.app"}]}`}, "/Apps/Bar.app"},
		{[]string{`{"app": ["Foo.app", {"target": "~/Applications/Foo.app"}]}`}, "/Users/me/Applications/Foo.app"},
		{[]string{`{"app": ["Foo.app", {"target": "/Opt/Foo.app"}]}`}, "/Opt/Foo.app"},
		{[]string{`{"app": ["A.app"]}`, `{"app": ["B.app"]}`}, ""},
		{[]string{`{"binary": ["op"]}`}, ""},
	} {
		var c api.Cask
		for _, a := range tc.artifacts {
			c.Artifacts = append(c.Artifacts, json.RawMessage(a))
		}
		got, ok := appTarget(c, "/Apps", "/Users/me")
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("appTarget(%v) = %q, %v; want %q", tc.artifacts, got, ok, tc.want)
		}
	}
}

func TestReadBundle(t *testing.T) {
	if _, err := os.Stat("/usr/bin/plutil"); err != nil {
		t.Skip("needs plutil")
	}
	app := filepath.Join(t.TempDir(), "X.app")
	plist := filepath.Join(app, "Contents", "Info.plist")
	if err := os.MkdirAll(filepath.Dir(plist), 0o755); err != nil {
		t.Fatal(err)
	}
	xml := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict><key>CFBundleShortVersionString</key><string>1.2.3</string></dict></plist>`
	if err := os.WriteFile(plist, []byte(xml), 0o644); err != nil {
		t.Fatal(err)
	}
	if short, long, ok := ReadBundle(app); !ok || short != "1.2.3" || long != "" {
		t.Errorf("ReadBundle = %q, %q, %v", short, long, ok)
	}
	if _, _, ok := ReadBundle(filepath.Join(t.TempDir(), "None.app")); ok {
		t.Error("an app without Info.plist should not be readable")
	}
}
