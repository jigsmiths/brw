package info

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
)

// rawEntry is an API entry as the server sends it, with the keys in this order.
const rawEntry = `{"name":"jq","full_name":"jq","tap":"homebrew/core","desc":"a < b & c","versions":{"stable":"1.0","head":null,"bottle":true},` +
	`"bottle":{"stable":{"rebuild":0,"root_url":"https://r","files":{"arm64_golden_gate":{"cellar":":any","url":"u1","sha256":"s1"},"arm64_tahoe":{"cellar":":any","url":"u2","sha256":"s2"}}}},` +
	`"dependencies":["a"],"caveats":"see $HOMEBREW_CELLAR/jq and $HOMEBREW_PREFIX/etc","installed":[],"linked_keg":null,"pinned":false,"outdated":false,` +
	`"service":{"run":["$HOMEBREW_PREFIX/opt/jq/bin/jq"],"log":"$HOMEBREW_CELLAR/x"},"executables":["jq"],"variations":{"arm64_golden_gate":{"dependencies":["a","b"]}},` +
	`"vulnerabilities":[],"ruby_source_path":"Formula/j/jq.rb"}`

func entryFor(t *testing.T, raw string, env Env) (*object, string) {
	t.Helper()
	fs, err := api.Parse([]byte("[" + raw + "]"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := JSON(json.RawMessage(raw), &fs[0], env)
	if err != nil {
		t.Fatal(err)
	}
	o, err := parseObject(out)
	if err != nil {
		t.Fatal(err)
	}
	return o, string(out)
}

func TestJSONKeysAndOverrides(t *testing.T) {
	prefix := t.TempDir()
	env := envFor(t, prefix, mac, index(t, base("jq", nil)))
	o, out := entryFor(t, rawEntry, env)

	wantKeys := []string{"name", "full_name", "tap", "desc", "versions", "bottle", "dependencies", "caveats",
		"installed", "linked_keg", "pinned", "outdated", "service", "ruby_source_path"}
	if !reflect.DeepEqual(o.keys, wantKeys) {
		t.Errorf("keys = %v\nwant   %v", o.keys, wantKeys)
	}
	if got := string(o.vals["dependencies"]); got != `["a","b"]` {
		t.Errorf("variation not applied: %s", got)
	}
	if got := string(o.vals["desc"]); got != `"a < b & c"` || strings.Contains(out, "\\u003c") {
		t.Errorf("desc was escaped: %s", got)
	}
	if got, want := string(o.vals["caveats"]), `"see $HOMEBREW_PREFIX/Cellar/jq and $HOMEBREW_PREFIX/etc"`; got != want {
		t.Errorf("caveats = %s, want %s", got, want)
	}
	wantService := `{"run":["` + prefix + `/opt/jq/bin/jq"],"log":"` + prefix + `/Cellar/x"}`
	if got := string(o.vals["service"]); got != wantService {
		t.Errorf("service = %s, want %s", got, wantService)
	}
	if got := string(o.vals["bottle"]); !strings.Contains(got, `"arm64_golden_gate"`) || strings.Contains(got, "arm64_tahoe") {
		t.Errorf("bottle should only hold this host's file: %s", got)
	}
}

func TestJSONVersionsBottleIsPerHost(t *testing.T) {
	idx := index(t, base("jq", nil))
	mac, _ := entryFor(t, rawEntry, envFor(t, t.TempDir(), mac, idx))
	linux, _ := entryFor(t, rawEntry, envFor(t, t.TempDir(), linux, idx))
	if got := string(mac.vals["versions"]); got != `{"stable":"1.0","head":null,"bottle":true}` {
		t.Errorf("mac versions = %s", got)
	}
	if got := string(linux.vals["versions"]); got != `{"stable":"1.0","head":null,"bottle":false}` {
		t.Errorf("linux versions = %s", got)
	}
}

func TestJSONVariationOnlyForThisHost(t *testing.T) {
	env := envFor(t, t.TempDir(), linux, index(t, base("jq", nil)))
	o, _ := entryFor(t, rawEntry, env)
	if got := string(o.vals["dependencies"]); got != `["a"]` {
		t.Errorf("dependencies = %s", got)
	}
	if got := string(o.vals["bottle"]); got != `{}` {
		t.Errorf("no bottle for this host should be {}, got %s", got)
	}
}

func TestJSONInstalledState(t *testing.T) {
	prefix := t.TempDir()
	install(t, prefix, "jq", "0.9", true, "oniguruma")
	install(t, prefix, "jq", "1.0", false)
	sbomFile := filepath.Join(prefix, "Cellar/jq/1.0/sbom.spdx.json")
	if err := os.WriteFile(sbomFile, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	link(t, prefix, "jq", "0.9")
	if err := os.MkdirAll(filepath.Join(prefix, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/jq/0.9", filepath.Join(prefix, "var/homebrew/pinned/jq")); err != nil {
		t.Fatal(err)
	}
	idx := index(t, base("jq", formula{"versions": map[string]any{"stable": "1.1"}}))
	o, _ := entryFor(t, rawEntry, envFor(t, prefix, mac, idx))

	if got := string(o.vals["linked_keg"]); got != `"0.9"` {
		t.Errorf("linked_keg = %s", got)
	}
	if got := string(o.vals["pinned"]); got != "true" {
		t.Errorf("pinned = %s", got)
	}
	if got := string(o.vals["outdated"]); got != "true" {
		t.Errorf("outdated = %s", got)
	}
	var installed []map[string]any
	if err := json.Unmarshal(o.vals["installed"], &installed); err != nil || len(installed) != 2 {
		t.Fatalf("installed = %s, %v", o.vals["installed"], err)
	}
	if installed[0]["version"] != "0.9" || installed[1]["version"] != "1.0" {
		t.Errorf("kegs should run oldest first: %v", installed)
	}
	first, _ := parseObject(o.vals["installed"][1:]) // the object after the opening bracket
	wantFirst := []string{"sbom", "version", "used_options", "built_as_bottle", "poured_from_bottle", "time", "runtime_dependencies", "installed_on_request"}
	if first == nil || !reflect.DeepEqual(first.keys, wantFirst) {
		t.Errorf("installed keys = %v, want %v", first, wantFirst)
	}
	if installed[0]["sbom"] != nil || installed[1]["sbom"] != sbomFile {
		t.Errorf("sbom = %v, %v, want null and %s", installed[0]["sbom"], installed[1]["sbom"], sbomFile)
	}
	if installed[0]["installed_on_request"] != true || installed[1]["installed_on_request"] != false {
		t.Errorf("installed_on_request = %v, %v", installed[0]["installed_on_request"], installed[1]["installed_on_request"])
	}
}

func TestJSONNotInstalled(t *testing.T) {
	o, _ := entryFor(t, rawEntry, envFor(t, t.TempDir(), mac, index(t, base("jq", nil))))
	if string(o.vals["installed"]) != "[]" || string(o.vals["linked_keg"]) != "null" ||
		string(o.vals["pinned"]) != "false" || string(o.vals["outdated"]) != "false" {
		t.Errorf("installed=%s linked=%s pinned=%s outdated=%s",
			o.vals["installed"], o.vals["linked_keg"], o.vals["pinned"], o.vals["outdated"])
	}
}

func TestParseObjectRejectsNonObjects(t *testing.T) {
	for _, in := range []string{"[]", "3", "{", ""} {
		if _, err := parseObject([]byte(in)); err == nil {
			t.Errorf("parseObject(%q) should fail", in)
		}
	}
}
