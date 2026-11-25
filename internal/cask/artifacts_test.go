package cask

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/platform"
)

func raws(t *testing.T, s string) []json.RawMessage {
	t.Helper()
	var out []json.RawMessage
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

var where = Where{Prefix: "/opt/homebrew", AppDir: "/Applications", Home: "/Users/me"}

func TestPlanVSCode(t *testing.T) {
	c := api.Cask{Token: "visual-studio-code", Artifacts: raws(t, `[
		{"uninstall":[{"launchctl":"com.microsoft.VSCode.ShipIt","quit":"com.microsoft.VSCode"}]},
		{"app":["Visual Studio Code.app"],"target":"/Applications/Visual Studio Code.app"},
		{"binary":["$APPDIR/Visual Studio Code.app/Contents/Resources/app/bin/code"],"target":"$HOMEBREW_PREFIX/bin/code"},
		{"zap":[{"trash":["~/.vscode"]}]}]`)}
	p, err := plan(c, where)
	if err != nil {
		t.Fatal(err)
	}
	if want := []move{{"Visual Studio Code.app", "/Applications/Visual Studio Code.app", "app"}}; !slices.Equal(p.apps, want) {
		t.Errorf("apps = %+v", p.apps)
	}
	if want := []move{{"/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code", "/opt/homebrew/bin/code", "binary"}}; !slices.Equal(p.links, want) {
		t.Errorf("links = %+v", p.links)
	}
	want := `[{"uninstall":[{"launchctl":"com.microsoft.VSCode.ShipIt","quit":"com.microsoft.VSCode"}]},` +
		`{"app":["Visual Studio Code.app"]},` +
		`{"binary":["/Applications/Visual Studio Code.app/Contents/Resources/app/bin/code"]},` +
		`{"zap":[{"trash":["~/.vscode"]}]}]`
	if got, _ := json.Marshal(p.uninstall); string(got) != want {
		t.Errorf("uninstall artifacts = %s\nwant %s", got, want)
	}
}

func TestPlanTargets(t *testing.T) {
	c := api.Cask{Token: "x", Artifacts: raws(t, `[
		{"app":["Foo.app",{"target":"Foo Beta.app"}]},
		{"app":["Bar.app",{"target":"~/Applications/Bar.app"}]},
		{"app":["Baz.app",{"target":"/Users/Shared/Baz.app"}]},
		{"binary":["foo-cli"]},
		{"binary":["bin/tool.sh",{"target":"tool"}]}]`)}
	p, err := plan(c, where)
	if err != nil {
		t.Fatal(err)
	}
	wantApps := []move{
		{"Foo.app", "/Applications/Foo Beta.app", "app"},
		{"Bar.app", "/Users/me/Applications/Bar.app", "app"},
		{"Baz.app", "/Users/Shared/Baz.app", "app"},
	}
	if !slices.Equal(p.apps, wantApps) {
		t.Errorf("apps = %+v", p.apps)
	}
	wantBins := []move{{"foo-cli", "/opt/homebrew/bin/foo-cli", "binary"}, {"bin/tool.sh", "/opt/homebrew/bin/tool", "binary"}}
	if !slices.Equal(p.links, wantBins) {
		t.Errorf("links = %+v", p.links)
	}
	if got, _ := json.Marshal(p.uninstall); !strings.Contains(string(got), `{"binary":["bin/tool.sh",{"target":"tool"}]}`) {
		t.Errorf("a binary's own options should stay in the receipt: %s", got)
	}
}

func TestPlanRefusesWhatBrwCannotInstall(t *testing.T) {
	for name, c := range map[string]api.Cask{
		"pkg":     {Token: "a", Artifacts: raws(t, `[{"pkg":["a.pkg"]}]`)},
		"suite":   {Token: "b", Artifacts: raws(t, `[{"app":["B.app"]},{"suite":["b"]}]`)},
		"pkgutil": {Token: "c", Artifacts: raws(t, `[{"uninstall":[{"pkgutil":"com.c"}]},{"app":["C.app"]}]`)},
		"rename":  {Token: "d", Artifacts: raws(t, `[{"app":["D.app"]}]`), Rename: raws(t, `[{"from":"a","to":"b"}]`)},
		"nothing": {Token: "h", Artifacts: raws(t, `[{"zap":[{"trash":"~/x"}]}]`)},
	} {
		if _, err := plan(c, where); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: err = %v, want ErrUnsupported", name, err)
		}
	}
}

func TestPlanAcceptsDependencies(t *testing.T) {
	c := api.Cask{Token: "e", Artifacts: raws(t, `[{"app":["E.app"]}]`),
		DependsOn: api.CaskDependsOn{Formula: []string{"jq"}, Cask: []string{"g"}}}
	if _, err := plan(c, where); err != nil {
		t.Errorf("err = %v, want a cask with dependencies planned", err)
	}
}

func TestCheckRequirements(t *testing.T) {
	mac := func(version, arch string) platform.Host {
		return platform.Host{OS: "darwin", MacVersion: version, Arch: arch}
	}
	atLeast := func(v string) api.CaskDependsOn {
		return api.CaskDependsOn{MacOS: &api.MacOSRequirement{Comparator: ">=", Versions: []string{v}}}
	}
	arm := api.CaskDependsOn{Arch: []struct {
		Type string `json:"type"`
		Bits int    `json:"bits"`
	}{{Type: "arm", Bits: 64}}}
	for _, tc := range []struct {
		deps api.CaskDependsOn
		host platform.Host
		want string
	}{
		{atLeast("12"), mac("15.1", "arm64"), ""},
		{atLeast("26"), mac("15.1", "arm64"), "x: This cask does not run on macOS versions older than Tahoe."},
		{atLeast("11"), mac("27.0", "arm64"), ""},
		{api.CaskDependsOn{MacOS: &api.MacOSRequirement{Comparator: "==", Versions: []string{"14", "15"}}}, mac("26.0", "arm64"),
			"x: This cask does not run on macOS versions other than Sonoma and Sequoia."},
		{api.CaskDependsOn{MacOS: &api.MacOSRequirement{Comparator: ">="}}, mac("26.0", "arm64"), ""},
		{arm, mac("26.0", "arm64"), ""},
		{arm, mac("26.0", "amd64"),
			"x: This cask depends on hardware architecture being one of [{type: :arm, bits: 64}], but you are running {type: :intel, bits: 64}."},
		{api.CaskDependsOn{}, platform.Host{OS: "linux", Arch: "amd64"}, "x: This cask requires macOS."},
	} {
		err := checkRequirements(api.Cask{Token: "x", DependsOn: tc.deps}, tc.host)
		got := ""
		if err != nil {
			got = err.Error()
		}
		if got != tc.want {
			t.Errorf("%+v on %+v: got %q, want %q", tc.deps, tc.host, got, tc.want)
		}
	}
}

func TestJSONFillerKeepsKeyOrder(t *testing.T) {
	fill := jsonFiller(Placeholders(where))
	in := `[{"quit":"com.x","delete":["$APPDIR/X.app",{"n":1.5,"b":true,"z":null}]}]`
	want := `[{"quit":"com.x","delete":["/Applications/X.app",{"n":1.5,"b":true,"z":null}]}]`
	if got := fill(json.RawMessage(in)); string(got) != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestPlaceholders(t *testing.T) {
	fill := Placeholders(where)
	if got := fill("$APPDIR/A.app $HOMEBREW_PREFIX/bin $HOMEBREW_CELLAR/x /$HOME/y"); got != "/Applications/A.app /opt/homebrew/bin /opt/homebrew/Cellar/x /Users/me/y" {
		t.Errorf("got %q", got)
	}
}
