package info

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/platform"
)

var (
	mac   = platform.Host{OS: "darwin", MacVersion: "27.0", Arch: "arm64"}
	linux = platform.Host{OS: "linux", Arch: "amd64"}
)

type formula map[string]any

// base fills the fields every API entry has.
func base(name string, extra formula) formula {
	f := formula{
		"name": name, "full_name": name, "tap": "homebrew/core",
		"versions": map[string]any{"stable": "1.0", "head": nil},
		"bottle": map[string]any{"stable": map[string]any{"rebuild": 0, "root_url": "https://r", "files": map[string]any{
			"arm64_golden_gate": map[string]any{"cellar": ":any", "url": "https://r/" + name, "sha256": "ab"},
			"x86_64_linux":      map[string]any{"cellar": ":any", "url": "https://r/" + name, "sha256": "cd"},
		}}},
		"ruby_source_path": "Formula/" + name[:1] + "/" + name + ".rb",
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func index(t *testing.T, formulae ...formula) *api.Index {
	t.Helper()
	b, err := json.Marshal(formulae)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := api.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewIndex(fs)
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// install creates a keg whose receipt says how it was installed, and links it.
func install(t *testing.T, prefix, name, version string, onRequest bool, deps ...string) {
	t.Helper()
	var runtime []map[string]any
	for _, d := range deps {
		runtime = append(runtime, map[string]any{"full_name": d, "version": "1", "revision": 0, "bottle_rebuild": 0, "pkg_version": "1", "declared_directly": true})
	}
	receipt, err := json.Marshal(map[string]any{
		"used_options": []string{}, "built_as_bottle": true, "poured_from_bottle": true,
		"installed_on_request": onRequest, "time": 1700000000, "runtime_dependencies": runtime,
		"source": map[string]any{"tap": "homebrew/core", "versions": map[string]any{"version_scheme": 0}},
	})
	if err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(prefix, "Cellar", name, version)
	writeFile(t, filepath.Join(keg, "INSTALL_RECEIPT.json"), string(receipt))
	writeFile(t, filepath.Join(keg, "bin", name), "x")
}

func link(t *testing.T, prefix, name, version string) {
	t.Helper()
	for _, dir := range []string{"opt", "var/homebrew/linked"} {
		rel := "../Cellar/" + name + "/" + version
		if dir != "opt" {
			rel = "../../../Cellar/" + name + "/" + version
		}
		if err := os.MkdirAll(filepath.Join(prefix, dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(rel, filepath.Join(prefix, dir, name)); err != nil {
			t.Fatal(err)
		}
	}
}

func envFor(t *testing.T, prefix string, host platform.Host, idx *api.Index) Env {
	t.Helper()
	kegs, err := cellar.Prefix(prefix).Kegs()
	if err != nil {
		t.Fatal(err)
	}
	stale, err := outdated.Check(idx, cellar.Prefix(prefix), kegs)
	if err != nil {
		t.Fatal(err)
	}
	return Env{Index: idx, Host: host, Prefix: cellar.Prefix(prefix), Kegs: kegs, Outdated: stale,
		Now: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)}
}

func text(t *testing.T, name string, env Env) string {
	t.Helper()
	f, ok := env.Index.Lookup(name)
	if !ok {
		t.Fatalf("%s not in index", name)
	}
	got, err := Text(f, env)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// sizes replaces "(N files, 1.2KB)" with a fixed text, since keg sizes depend on the filesystem.
func sizes(s string) string {
	var out []string
	for line := range strings.SplitSeq(s, "\n") {
		if i := strings.Index(line, " ("); i >= 0 && strings.Contains(line, "B)") {
			j := strings.Index(line, ")")
			line = line[:i] + " (SIZE" + line[j:]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestNotInstalled(t *testing.T) {
	idx := index(t, base("zsh-lovers", formula{
		"desc": "Tips, tricks, and examples for zsh", "homepage": "https://grml.org/zsh/#zshlovers", "license": "GPL-2.0-only",
	}))
	got := text(t, "zsh-lovers", envFor(t, t.TempDir(), mac, idx))
	want := `==> zsh-lovers: stable 1.0 (bottled)
Tips, tricks, and examples for zsh
https://grml.org/zsh/#zshlovers
Not installed
From: https://github.com/Homebrew/homebrew-core/blob/HEAD/Formula/z/zsh-lovers.rb
License: GPL-2.0-only
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestInstalledSections(t *testing.T) {
	prefix := t.TempDir()
	install(t, prefix, "jq", "1.0", true, "oniguruma")
	link(t, prefix, "jq", "1.0")
	install(t, prefix, "oniguruma", "1", false)
	idx := index(t,
		base("jq", formula{
			"desc": "JSON processor", "aliases": []string{"jaq"}, "oldnames": []string{"jqold"},
			"versions":     map[string]any{"stable": "1.0", "head": "HEAD"},
			"dependencies": []string{"oniguruma"}, "build_dependencies": []string{"autoconf"},
			"recommended_dependencies": []string{"rec"}, "optional_dependencies": []string{"opt"},
		}),
		base("oniguruma", nil), base("rec", nil), base("opt", nil), base("autoconf", nil))
	got := sizes(text(t, "jq", envFor(t, prefix, mac, idx)))
	want := `==> jq: stable 1.0 (bottled), HEAD
JSON processor
Aliases: jaq
Old Names: jqold
Installed (on request)
From: https://github.com/Homebrew/homebrew-core/blob/HEAD/Formula/j/jq.rb
==> Installed Versions
jq 1.0 (SIZE) [Linked]
==> Dependencies
Required (1): oniguruma
Recommended (1): rec
Optional (1): opt
Recursive Runtime (1): all installed ✔
==> Options
--HEAD
	Install HEAD version
`
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestInstalledAsDependencyAndMissingRuntimeDependency(t *testing.T) {
	prefix := t.TempDir()
	install(t, prefix, "lib", "1.0", false, "gone", "other")
	link(t, prefix, "lib", "1.0")
	install(t, prefix, "other", "1", false)
	got := text(t, "lib", envFor(t, prefix, mac, index(t, base("lib", nil), base("other", nil))))
	if !strings.Contains(got, "Installed (as dependency)\n") {
		t.Errorf("status missing in\n%s", got)
	}
	if !strings.Contains(got, "Recursive Runtime (2): 1 installed ✔, 1 missing ✘\n") {
		t.Errorf("runtime line missing in\n%s", got)
	}
}

func TestBuildDependenciesShownOnlyWhenBuilt(t *testing.T) {
	prefix := t.TempDir()
	install(t, prefix, "bottled", "1.0", true)
	install(t, prefix, "source", "1.0", true)
	receipt := filepath.Join(prefix, "Cellar/source/1.0/INSTALL_RECEIPT.json")
	b, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, receipt, strings.Replace(string(b), `"poured_from_bottle":true`, `"poured_from_bottle":false`, 1))

	build := formula{"build_dependencies": []string{"cmake"}}
	idx := index(t, base("bottled", build), base("source", build), base("fresh", build),
		base("nobottle", formula{"build_dependencies": []string{"cmake"}, "bottle": map[string]any{}}),
		base("linuxonly", formula{"build_dependencies": []string{"cmake"}, "bottle": map[string]any{},
			"requirements": []map[string]any{{"name": "linux"}}}),
		base("cmake", nil))
	env := envFor(t, prefix, mac, idx)
	tests := map[string]bool{
		"bottled":   false, // poured from a bottle
		"source":    true,  // built here
		"fresh":     false, // not installed, but a bottle exists
		"nobottle":  true,  // not installed and no bottle
		"linuxonly": false, // not installed and only for another OS
	}
	for name, want := range tests {
		if got := strings.Contains(text(t, name, env), "Build (1): cmake"); got != want {
			t.Errorf("%s: build line shown = %v, want %v", name, got, want)
		}
	}
}

func TestKegOnlyUsesPlatformVariation(t *testing.T) {
	idx := index(t, base("tool", formula{
		"keg_only":   false,
		"variations": map[string]any{"arm64_golden_gate": map[string]any{"keg_only": true}},
	}))
	if got := text(t, "tool", envFor(t, t.TempDir(), mac, idx)); !strings.Contains(got, "(bottled) [keg-only]\n") {
		t.Errorf("mac:\n%s", got)
	}
	if got := text(t, "tool", envFor(t, t.TempDir(), linux, idx)); strings.Contains(got, "keg-only") {
		t.Errorf("linux:\n%s", got)
	}
}

func TestOutdatedHeaderAndRow(t *testing.T) {
	prefix := t.TempDir()
	install(t, prefix, "jq", "1.0", true)
	link(t, prefix, "jq", "1.0")
	idx := index(t, base("jq", formula{"versions": map[string]any{"stable": "1.2", "head": nil}}))
	got := sizes(text(t, "jq", envFor(t, prefix, mac, idx)))
	if !strings.HasPrefix(got, "==> jq: 1.0 → stable 1.2 (bottled)\n") {
		t.Errorf("header:\n%s", got)
	}
	if !strings.Contains(got, "\njq 1.0 → 1.2 (SIZE) [Linked]\n") {
		t.Errorf("row:\n%s", got)
	}
}

func TestInstalledVersionsRelatedFormulae(t *testing.T) {
	prefix := t.TempDir()
	install(t, prefix, "ssl@3", "3.6", false)
	install(t, prefix, "ssl@4", "4.0", false)
	link(t, prefix, "ssl@4", "4.0")
	install(t, prefix, "ssl@3", "3.5", false) // an older keg that is not linked is hidden
	idx := index(t,
		base("ssl@3", formula{"versions": map[string]any{"stable": "3.6"}, "versioned_formulae": []string{"ssl@4"}}),
		base("ssl@4", formula{"versions": map[string]any{"stable": "4.0"}}))
	got := sizes(text(t, "ssl@3", envFor(t, prefix, mac, idx)))
	want := "==> Installed Versions\nssl@4 4.0 (SIZE) [Linked]\nssl@3 3.6 (SIZE)\n"
	if !strings.Contains(got, want) {
		t.Errorf("want\n%s\nin\n%s", want, got)
	}
}

func TestFormatRows(t *testing.T) {
	got := formatRows([]row{
		{"openssl@4", "4.0.3", "(6,820 files, 23.8MB)", "[Linked]"},
		{"openssl@3", "3.6.5", "(7,642 files, 37.8MB)", ""},
		{"x", "10.0.0 → 10.0.1", "(1 files)", "[Linked]"},
	})
	want := []string{
		"openssl@4 4.0.3           (6,820 files, 23.8MB) [Linked]",
		"openssl@3 3.6.5           (7,642 files, 37.8MB)",
		"x         10.0.0 → 10.0.1 (1 files)             [Linked]",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestConflicts(t *testing.T) {
	reason := "both install `ndiff` binaries"
	idx := index(t,
		base("nmap", formula{"conflicts_with": []string{"ndiff", "cern-ndiff", "nmap", "ghost"},
			"conflicts_with_reasons": []any{reason, reason, nil, nil}}),
		base("ndiff", nil))
	got := text(t, "nmap", envFor(t, t.TempDir(), mac, idx))
	want := "Conflicts with:\n  cern-ndiff (because both install `ndiff` binaries)\n  ghost\n  ndiff (because both install `ndiff` binaries)\n"
	if !strings.Contains(got, want) {
		t.Errorf("want\n%s\nin\n%s", want, got)
	}
}

func TestDeprecation(t *testing.T) {
	tests := []struct {
		name string
		f    formula
		want string
	}{
		{"deprecated with a known reason", formula{"deprecated": true, "deprecation_reason": "unmaintained", "deprecation_date": "2026-07-17", "disable_date": "2027-01-05"},
			"Deprecated because it is not maintained upstream! It will be disabled on 2027-01-05."},
		{"disable date derived a year after deprecation", formula{"deprecated": true, "deprecation_reason": "repo_archived", "deprecation_date": "2026-06-05"},
			"Deprecated because it has an archived upstream repository! It will be disabled on 2027-06-05."},
		{"leap day clamps", formula{"deprecated": true, "deprecation_date": "2024-02-29"},
			"Deprecated! It was disabled on 2025-02-28."},
		{"disabled in the past with free text and a replacement", formula{"disabled": true, "disable_reason": "needs `gsl`", "disable_date": "2026-05-16", "disable_replacement_formula": "gnu-gsl"},
			"Disabled because it needs `gsl`! It was disabled on 2026-05-16.\nReplacement:\n  brew install --formula gnu-gsl\n"},
		{"cask replacement", formula{"deprecated": true, "deprecation_reason": "unsupported", "disable_date": "2026-12-18", "deprecation_replacement_cask": "cli"},
			"Deprecated because it is not supported upstream! It will be disabled on 2026-12-18.\nReplacement:\n  brew install --cask cli\n"},
		{"same name as formula and cask", formula{"deprecated": true, "disable_date": "2030-01-01", "deprecation_replacement_formula": "x", "deprecation_replacement_cask": "x"},
			"Deprecated! It will be disabled on 2030-01-01.\nReplacement:\n  brew install x\n"},
		{"neither", formula{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := base("pkg", tt.f)
			env := envFor(t, t.TempDir(), mac, index(t, f))
			p, _ := env.Index.Lookup("pkg")
			if got := env.deprecation(p); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCaveatsExpandPrefix(t *testing.T) {
	prefix := t.TempDir()
	idx := index(t, base("pkg", formula{"caveats": "Put files in $HOMEBREW_PREFIX/etc and $HOMEBREW_CELLAR/pkg.\n"}))
	got := text(t, "pkg", envFor(t, prefix, mac, idx))
	want := "==> Caveats\nPut files in " + prefix + "/etc and " + prefix + "/Cellar/pkg.\n"
	if !strings.HasSuffix(got, want) {
		t.Errorf("want\n%s\nin\n%s", want, got)
	}
}

func TestInstalledFromAnotherTap(t *testing.T) {
	prefix := t.TempDir()
	install(t, prefix, "bun", "1.0", true)
	receipt := filepath.Join(prefix, "Cellar/bun/1.0/INSTALL_RECEIPT.json")
	b, err := os.ReadFile(receipt)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, receipt, strings.Replace(string(b), `"tap":"homebrew/core"`, `"tap":"oven-sh/bun"`, 1))
	idx := index(t, base("bun", nil))
	f, _ := idx.Lookup("bun")
	if _, err := Text(f, envFor(t, prefix, mac, idx)); !errors.Is(err, ErrOtherTap) {
		t.Errorf("error = %v, want ErrOtherTap", err)
	}
}

func TestPlusYear(t *testing.T) {
	for in, want := range map[string]string{"2026-06-05": "2027-06-05", "2024-02-29": "2025-02-28", "2023-12-31": "2024-12-31"} {
		d, _ := time.Parse(time.DateOnly, in)
		if got := plusYear(d).Format(time.DateOnly); got != want {
			t.Errorf("plusYear(%s) = %s, want %s", in, got, want)
		}
	}
}

func TestCaveatsHelper(t *testing.T) {
	f := &api.Formula{Caveats: "Use $HOMEBREW_PREFIX/etc and $HOMEBREW_CELLAR/x.\n"}
	if got, want := Caveats(f, "/opt/brw"), "Use /opt/brw/etc and /opt/brw/Cellar/x.\n"; got != want {
		t.Errorf("Caveats = %q, want %q", got, want)
	}
	if got := Caveats(&api.Formula{}, "/opt/brw"); got != "" {
		t.Errorf("Caveats of a formula without any = %q", got)
	}
}
