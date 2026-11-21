package trust

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPath(t *testing.T) {
	t.Setenv("HOME", "/Users/me")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOMEBREW_XDG_CONFIG_HOME", "")
	t.Setenv("HOMEBREW_USER_CONFIG_HOME", "/ignored") // brew sets this itself
	if got := Path(); got != "/Users/me/.homebrew/trust.json" {
		t.Errorf("Path = %s", got)
	}
	t.Setenv("HOMEBREW_XDG_CONFIG_HOME", "/hb-xdg")
	if got := Path(); got != "/hb-xdg/homebrew/trust.json" {
		t.Errorf("Path with HOMEBREW_XDG_CONFIG_HOME = %s", got)
	}
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got := Path(); got != "/xdg/homebrew/trust.json" {
		t.Errorf("Path with XDG_CONFIG_HOME = %s", got)
	}
}

func TestReference(t *testing.T) {
	for _, tc := range []struct{ remote, want string }{
		{"", "jigsmiths/tap"},
		{"https://github.com/jigsmiths/homebrew-tap", "jigsmiths/tap"},
		{"https://github.com/JigSmiths/homebrew-tap.git/", "jigsmiths/tap"},
		{"git@github.com:jigsmiths/homebrew-tap.git", "jigsmiths/tap"},
		{"https://github.com/someone/fork", "https://github.com/someone/fork"},
		{"/Users/Me/origin", "/users/me/origin"},
	} {
		if got := Reference("jigsmiths/tap", tc.remote); got != tc.want {
			t.Errorf("Reference(%q) = %q, want %q", tc.remote, got, tc.want)
		}
	}
}

const sample = `{
  "trustedformulae": [
    "someone/tap/tool"
  ],
  "trustedtaps": [
    "/users/me/origin",
    "trusted/tap"
  ],
  "trustedcasks": [
    "/users/me/mirror/app",
    "partly/tap/app"
  ]
}
`

func load(t *testing.T, content string) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "homebrew", "trust.json")
	if content != "" {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestTrusted(t *testing.T) {
	s := load(t, sample)
	// An empty remote is a tap on its default GitHub remote.
	for _, tc := range []struct {
		tap, remote, token string
		tapOK, caskOK      bool
	}{
		{"homebrew/cask", "", "slack", true, true},
		{"trusted/tap", "", "any", true, true},
		{"Trusted/Tap", "https://github.com/trusted/homebrew-tap.git", "any", true, true},
		// The name only stands for the tap on its own remote.
		{"trusted/tap", "https://example.com/trusted.git", "any", false, false},
		{"partly/tap", "", "app", false, true},
		{"partly/tap", "", "other", false, false},
		{"local/tap", "/Users/me/origin", "any", true, true},
		{"mirror/tap", "/users/me/mirror", "app", false, true},
		{"mirror/tap", "/users/me/mirror", "other", false, false},
		{"untrusted/tap", "", "app", false, false},
	} {
		if got := s.TapTrusted(tc.tap, tc.remote); got != tc.tapOK {
			t.Errorf("TapTrusted(%s, %q) = %v", tc.tap, tc.remote, got)
		}
		if got := s.CaskTrusted(tc.tap, tc.remote, tc.token); got != tc.caskOK {
			t.Errorf("CaskTrusted(%s, %q, %s) = %v", tc.tap, tc.remote, tc.token, got)
		}
	}
	if s.HasCasksFrom("partly/tap", "") != true || s.HasCasksFrom("untrusted/tap", "") {
		t.Error("HasCasksFrom should tell a partly trusted tap from an untrusted one")
	}
}

func TestRequired(t *testing.T) {
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "")
	if !Required() {
		t.Error("trust should be required by default")
	}
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "1")
	if Required() {
		t.Error("HOMEBREW_NO_REQUIRE_TAP_TRUST should turn it off")
	}
}

func TestTrustAndUntrustWriteBrewsFormat(t *testing.T) {
	s := load(t, sample)
	if added, err := s.Trust(KindCask, "Jigsmiths/Tap/brw"); err != nil || !added {
		t.Fatalf("Trust = %v, %v", added, err)
	}
	if added, err := s.Trust(KindCask, "jigsmiths/tap/brw"); err != nil || added {
		t.Errorf("Trust again = %v, %v", added, err)
	}
	if removed, err := s.Untrust(KindTap, "trusted/tap"); err != nil || !removed {
		t.Errorf("Untrust = %v, %v", removed, err)
	}
	if added, err := s.Trust(KindTap, "new/tap"); err != nil || !added {
		t.Errorf("Trust tap = %v, %v", added, err)
	}
	b, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	// Keys keep their order, entries are sorted, as brew writes them.
	want := `{
  "trustedformulae": [
    "someone/tap/tool"
  ],
  "trustedtaps": [
    "/users/me/origin",
    "new/tap"
  ],
  "trustedcasks": [
    "/users/me/mirror/app",
    "jigsmiths/tap/brw",
    "partly/tap/app"
  ]
}
`
	if string(b) != want {
		t.Errorf("trust.json =\n%s\nwant\n%s", b, want)
	}
	if fi, err := os.Stat(s.path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, %v", fi, err)
	}
}

func TestTrustMakesAPrivateFolderAndRemovesAnEmptyFile(t *testing.T) {
	s := load(t, "")
	if _, err := s.Trust(KindTap, "jigsmiths/tap"); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(filepath.Dir(s.path)); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("folder mode = %v, %v", fi, err)
	}
	if b, _ := os.ReadFile(s.path); string(b) != "{\n  \"trustedtaps\": [\n    \"jigsmiths/tap\"\n  ]\n}\n" {
		t.Errorf("trust.json = %q", b)
	}
	if _, err := s.Untrust(KindTap, "jigsmiths/tap"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path); err == nil {
		t.Error("an empty trust store should be removed, as brew does")
	}
}

func TestTrustRefusesAnInsecureStore(t *testing.T) {
	s := load(t, sample)
	if err := os.Chmod(filepath.Dir(s.path), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Trust(KindTap, "x/y"); err == nil {
		t.Error("wrote to a world-writable folder")
	}
	if err := os.Chmod(filepath.Dir(s.path), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(s.path), "link.json")
	if err := os.Symlink(s.path, link); err != nil {
		t.Fatal(err)
	}
	ls, err := Load(link)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ls.Trust(KindTap, "x/y"); err == nil {
		t.Error("wrote through a symlink")
	}
}

func TestLoadToleratesAGarbledStore(t *testing.T) {
	if s := load(t, "{"); s.TapTrusted("trusted/tap", "") {
		t.Error("a garbled store trusts nothing, as brew reads it")
	}
}
