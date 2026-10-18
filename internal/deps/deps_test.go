package deps

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/platform"
)

var (
	macHost   = platform.Host{OS: "darwin", MacVersion: "27.0", Arch: "arm64"}
	oldMac    = platform.Host{OS: "darwin", MacVersion: "12.7", Arch: "arm64"}
	linuxHost = platform.Host{OS: "linux", Arch: "amd64"}
)

type formula map[string]any

// index builds an API index from minimal formula definitions.
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

func resolver(t *testing.T, idx *api.Index, host platform.Host) *Resolver {
	t.Helper()
	r, err := NewResolver(idx, host)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func f(name string, deps ...string) formula {
	return formula{"name": name, "dependencies": deps}
}

func TestDirectIsSortedUniqueAndCanonical(t *testing.T) {
	idx := index(t,
		f("a", "d", "c", "bee", "c"),
		formula{"name": "b", "aliases": []string{"bee"}},
		f("c"), f("d"))
	got, err := resolver(t, idx, macHost).Direct("a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "c", "d"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Direct = %v, want %v", got, want)
	}
}

func TestRecursive(t *testing.T) {
	idx := index(t, f("a", "b", "c"), f("b", "d"), f("c", "d", "e"), f("d"), f("e"), f("unrelated", "a"))
	got, err := resolver(t, idx, macHost).Recursive("a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "c", "d", "e"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Recursive = %v, want %v", got, want)
	}
}

// Brew records runtime dependencies with the dependencies of each one before it,
// and every name once, where it first came up.
func TestExpandedPutsDependenciesBeforeTheirDependents(t *testing.T) {
	idx := index(t, f("app", "lib", "tool"), f("lib", "base"), f("tool", "base", "extra"), f("base"), f("extra"))
	got, err := resolver(t, idx, macHost).Expanded("app")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"base", "lib", "extra", "tool"}; !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestExpandedSurvivesACycleAndSkipsTheRoot(t *testing.T) {
	idx := index(t, f("a", "b"), f("b", "a"))
	got, err := resolver(t, idx, macHost).Expanded("a")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"b"}) {
		t.Errorf("got %v", got)
	}
}

func TestRecursiveWithCycle(t *testing.T) {
	idx := index(t, f("a", "b"), f("b", "c"), f("c", "b"))
	got, err := resolver(t, idx, macHost).Recursive("a")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"b", "c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Recursive = %v, want %v", got, want)
	}
}

func TestRecursiveNeverIncludesRoot(t *testing.T) {
	idx := index(t, f("a", "b"), f("b", "a"))
	got, err := resolver(t, idx, macHost).Recursive("a")
	if err != nil || !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("Recursive = %v, %v; want [b]", got, err)
	}
}

func TestMissingDependency(t *testing.T) {
	idx := index(t, f("a", "ghost"))
	_, err := resolver(t, idx, macHost).Recursive("a")
	if err == nil || !strings.Contains(err.Error(), "ghost") || !strings.Contains(err.Error(), "a") {
		t.Errorf("error = %v, want one naming ghost and a", err)
	}
}

func TestUnknownFormula(t *testing.T) {
	r := resolver(t, index(t, f("a")), macHost)
	if _, err := r.Direct("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Direct error = %v, want ErrNotFound", err)
	}
	if _, err := r.Recursive("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Recursive error = %v, want ErrNotFound", err)
	}
	if _, err := r.Tree("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Tree error = %v, want ErrNotFound", err)
	}
}

func TestLookupByAlias(t *testing.T) {
	idx := index(t, formula{"name": "node", "aliases": []string{"nodejs"}, "dependencies": []string{"libuv"}}, f("libuv"))
	got, err := resolver(t, idx, macHost).Direct("nodejs")
	if err != nil || !reflect.DeepEqual(got, []string{"libuv"}) {
		t.Errorf("Direct(nodejs) = %v, %v", got, err)
	}
}

func TestVariationForHostTag(t *testing.T) {
	idx := index(t, formula{
		"name": "a", "dependencies": []string{"b"},
		"variations": map[string]any{"x86_64_linux": map[string]any{"dependencies": []string{"b", "c"}}},
	}, f("b"), f("c"))
	linux, _ := resolver(t, idx, linuxHost).Direct("a")
	mac, _ := resolver(t, idx, macHost).Direct("a")
	if !reflect.DeepEqual(linux, []string{"b", "c"}) || !reflect.DeepEqual(mac, []string{"b"}) {
		t.Errorf("linux = %v, mac = %v", linux, mac)
	}
}

func TestUsesFromMacOS(t *testing.T) {
	idx := index(t, formula{
		"name":                   "a",
		"uses_from_macos":        []any{"sys", map[string]any{"buildonly": "build"}, "newer", "later"},
		"uses_from_macos_bounds": []any{map[string]any{}, map[string]any{}, map[string]any{"since": "ventura"}, map[string]any{"since": "sonoma"}},
	}, f("sys"), f("buildonly"), f("newer"), f("later"))

	tests := []struct {
		name string
		host platform.Host
		want []string
	}{
		{"linux needs every runtime entry", linuxHost, []string{"later", "newer", "sys"}},
		{"current macOS provides them all", macHost, nil},
		{"macOS 12 predates both bounds", oldMac, []string{"later", "newer"}},
		{"macOS 13 meets ventura but not sonoma", platform.Host{OS: "darwin", MacVersion: "13.6", Arch: "arm64"}, []string{"later"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolver(t, idx, tt.host).Direct("a")
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Direct = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNewResolverUnsupportedHost(t *testing.T) {
	if _, err := NewResolver(index(t, f("a")), platform.Host{OS: "windows", Arch: "amd64"}); err == nil {
		t.Error("expected an error")
	}
}

// Brew lists tree children in declaration order, unlike the flat list.
func TestTree(t *testing.T) {
	idx := index(t, f("a", "c", "b"), f("b", "d"), f("c", "d", "e"), f("d"), f("e"))
	got, err := resolver(t, idx, macHost).Tree("a")
	if err != nil {
		t.Fatal(err)
	}
	want := `a
├── c
│   ├── d
│   └── e
└── b
    └── d
`
	if got != want {
		t.Errorf("Tree =\n%s\nwant\n%s", got, want)
	}
}

func TestTreeLeafAndCycle(t *testing.T) {
	r := resolver(t, index(t, f("a"), f("x", "y"), f("y", "x")), macHost)
	if got, err := r.Tree("a"); err != nil || got != "a\n" {
		t.Errorf("Tree(leaf) = %q, %v", got, err)
	}
	if _, err := r.Tree("x"); err == nil {
		t.Error("expected an error for a dependency cycle")
	}
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func realResolver(t *testing.T) *Resolver {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "api", "testdata", "formulae.json"))
	if err != nil {
		t.Fatal(err)
	}
	fs, err := api.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return resolver(t, api.NewIndex(fs), macHost)
}

// The golden files are brew's own output on macOS 27 arm64.
func TestMatchesBrewOnRealFormulae(t *testing.T) {
	r := realResolver(t)
	for _, name := range []string{"python@3.13", "git"} {
		t.Run(name, func(t *testing.T) {
			all, err := r.Recursive(name)
			if err != nil {
				t.Fatal(err)
			}
			if got, want := strings.Join(all, "\n")+"\n", golden(t, "deps-"+name+".txt"); got != want {
				t.Errorf("Recursive =\n%swant\n%s", got, want)
			}
			tree, err := r.Tree(name)
			if err != nil {
				t.Fatal(err)
			}
			if want := strings.TrimRight(golden(t, "tree-"+name+".txt"), "\n") + "\n"; tree != want {
				t.Errorf("Tree =\n%swant\n%s", tree, want)
			}
		})
	}
}

func kegWith(name string, deps ...cellar.Dependency) cellar.Keg {
	return cellar.Keg{Name: name, Version: "1", Receipt: &cellar.Receipt{RuntimeDependencies: deps}}
}

func dep(name string, direct bool) cellar.Dependency {
	return cellar.Dependency{FullName: name, DeclaredDirectly: direct}
}

func TestInstalled(t *testing.T) {
	kegs := []cellar.Keg{
		kegWith("git", dep("pcre2", true), dep("json-c", false), dep("gettext", true)),
		kegWith("bare"),
		{Name: "norecord", Version: "1"},
	}
	got, err := Installed(kegs, "git")
	if err != nil || !reflect.DeepEqual(got, []string{"gettext", "json-c", "pcre2"}) {
		t.Errorf("Installed(git) = %v, %v", got, err)
	}
	if got, err := Installed(kegs, "bare"); err != nil || len(got) != 0 {
		t.Errorf("Installed(bare) = %v, %v", got, err)
	}
	if _, err := Installed(kegs, "norecord"); err == nil {
		t.Error("a keg without a receipt should be an error")
	}
	if _, err := Installed(kegs, "missing"); !errors.Is(err, ErrNotInstalled) {
		t.Errorf("error = %v, want ErrNotInstalled", err)
	}
}

func TestInstalledUsesIncludesIndirectDependents(t *testing.T) {
	kegs := []cellar.Keg{
		kegWith("app", dep("lib", true), dep("base", false)),
		kegWith("lib", dep("base", true)),
		kegWith("base"),
		kegWith("zed", dep("base", true)),
		{Name: "norecord", Version: "1"},
	}
	if got := InstalledUses(kegs, "base"); !reflect.DeepEqual(got, []string{"app", "lib", "zed"}) {
		t.Errorf("got %v, want every formula with base in its receipt", got)
	}
	if got := InstalledUses(kegs, "app"); len(got) != 0 {
		t.Errorf("nothing uses app, got %v", got)
	}
}

func TestInstalledUsesNamesTapFormulaeInFull(t *testing.T) {
	tapped := kegWith("zeta", dep("base", true))
	tapped.Receipt.Source.Tap = "me/tap"
	core := kegWith("alpha", dep("base", true))
	core.Receipt.Source.Tap = "homebrew/core"
	kegs := []cellar.Keg{tapped, core, kegWith("mid", dep("base", true))}
	want := []string{"alpha", "me/tap/zeta", "mid"}
	if got := InstalledUses(kegs, "base"); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v (full name for tap formulae, sorted by it)", got, want)
	}
}

func TestInstalledUsesCountsMultipleVersionsOnce(t *testing.T) {
	kegs := []cellar.Keg{kegWith("app", dep("base", true)), kegWith("app", dep("base", true))}
	if got := InstalledUses(kegs, "base"); !reflect.DeepEqual(got, []string{"app"}) {
		t.Errorf("got %v", got)
	}
}

func TestUses(t *testing.T) {
	idx := index(t, f("a", "b"), f("b", "c"), f("c"), f("d", "c"), f("e"))
	r := resolver(t, idx, macHost)
	direct, err := r.Uses("c", false)
	if err != nil || !reflect.DeepEqual(direct, []string{"b", "d"}) {
		t.Errorf("direct = %v, %v", direct, err)
	}
	all, err := r.Uses("c", true)
	if err != nil || !reflect.DeepEqual(all, []string{"a", "b", "d"}) {
		t.Errorf("recursive = %v, %v", all, err)
	}
	if none, err := r.Uses("e", true); err != nil || len(none) != 0 {
		t.Errorf("uses of e = %v, %v", none, err)
	}
	if _, err := r.Uses("nope", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestUsesToleratesUnresolvableDependencies(t *testing.T) {
	idx := index(t, f("a", "ghost", "b"), f("b"))
	got, err := resolver(t, idx, macHost).Uses("b", false)
	if err != nil || !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("Uses = %v, %v", got, err)
	}
}

func TestUsesFollowsAliases(t *testing.T) {
	idx := index(t, f("a", "bee"), formula{"name": "b", "aliases": []string{"bee"}})
	got, err := resolver(t, idx, macHost).Uses("bee", false)
	if err != nil || !reflect.DeepEqual(got, []string{"a"}) {
		t.Errorf("Uses(alias) = %v, %v", got, err)
	}
}

func TestTyped(t *testing.T) {
	idx := index(t, formula{
		"name":                     "app",
		"dependencies":             []string{"lib", "tool", "lib"},
		"build_dependencies":       []string{"cmake"},
		"recommended_dependencies": []string{"rec"},
		"optional_dependencies":    []string{"opt"},
		"uses_from_macos":          []any{"zlib", map[string]string{"bison": "build"}, map[string]string{"check": "test"}},
		"uses_from_macos_bounds":   []any{map[string]string{}, map[string]string{}, map[string]string{}},
	})
	// On Linux the system libraries are real dependencies, except test only ones.
	got, err := resolver(t, idx, linuxHost).Typed("app")
	if err != nil {
		t.Fatal(err)
	}
	want := Typed{
		Build:       []string{"cmake", "bison"},
		Required:    []string{"lib", "tool", "zlib"},
		Recommended: []string{"rec"},
		Optional:    []string{"opt"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("linux = %+v, want %+v", got, want)
	}
	// macOS provides them itself.
	got, err = resolver(t, idx, macHost).Typed("app")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lib", "tool"}; !reflect.DeepEqual(got.Required, want) || !reflect.DeepEqual(got.Build, []string{"cmake"}) {
		t.Errorf("macOS = %+v", got)
	}
	if _, err := resolver(t, idx, macHost).Typed("nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
