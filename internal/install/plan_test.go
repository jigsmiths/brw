package install

import (
	"cmp"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/deps"
	"github.com/jigsmiths/brw/internal/platform"
)

var (
	mac   = platform.Host{OS: "darwin", MacVersion: "26.0", Arch: "arm64"}
	linux = platform.Host{OS: "linux", Arch: "amd64"}
)

type formula map[string]any

// f is an API entry with a bottle for the Mac tags and Linux, a version and dependencies.
func f(name string, depNames ...string) formula {
	return formula{
		"name": name, "full_name": name, "tap": "homebrew/core",
		"versions": map[string]any{"stable": "1.0"}, "dependencies": depNames,
		"bottle": map[string]any{"stable": map[string]any{"files": map[string]any{
			"arm64_tahoe": map[string]any{}, "x86_64_linux": map[string]any{},
		}}},
	}
}

func with(base formula, key string, value any) formula {
	base[key] = value
	return base
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

// fakeBottles answers like the API client does, with a cellar per formula.
type fakeBottles map[string]string

func (b fakeBottles) Bottles(want map[string]string) (map[string]api.BottleFile, error) {
	out := map[string]api.BottleFile{}
	for name, tag := range want {
		cellar, ok := b[name]
		if !ok {
			cellar = cmp.Or(b["*"], ":any")
		}
		out[name] = api.BottleFile{Cellar: cellar, URL: "https://r/" + name + "/blobs/sha256:" + tag, SHA256: tag}
	}
	return out, nil
}

func planner(t *testing.T, host platform.Host, idx *api.Index) *Planner {
	t.Helper()
	p := &Planner{Index: idx, Host: host, Prefix: "/opt/homebrew", Bottles: fakeBottles{}, Linked: func(string) bool { return false }}
	if host.OS == "linux" { // only bottles that need no relocation can be installed there
		p.Prefix, p.Bottles = "/home/linuxbrew/.linuxbrew", fakeBottles{"*": ":any_skip_relocation"}
	}
	return p
}

func names(p *Plan) []string {
	var out []string
	for _, it := range p.Items {
		out = append(out, it.Formula.Name)
	}
	return out
}

func keg(name, version string) cellar.Keg {
	return cellar.Keg{Name: name, Version: version, Receipt: &cellar.Receipt{Source: cellar.Source{Tap: "homebrew/core"}}}
}

func TestPlanOrdersDependenciesFirst(t *testing.T) {
	idx := index(t, f("app", "lib", "tool"), f("lib", "base"), f("tool", "base"), f("base"))
	plan, err := planner(t, mac, idx).Plan([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	got := names(plan)
	if !slices.Equal(got, []string{"base", "lib", "tool", "app"}) {
		t.Errorf("order = %v, want dependencies before dependents, each once", got)
	}
	for _, it := range plan.Items {
		if it.OnRequest != (it.Formula.Name == "app") {
			t.Errorf("%s OnRequest = %v", it.Formula.Name, it.OnRequest)
		}
		if it.Tag != "arm64_tahoe" {
			t.Errorf("%s tag = %s", it.Formula.Name, it.Tag)
		}
	}
}

func TestPlanSkipsWhatIsInstalled(t *testing.T) {
	idx := index(t, f("app", "lib"), f("lib", "base"), f("base"))
	p := planner(t, mac, idx)
	p.Kegs = []cellar.Keg{keg("lib", "0.9")}
	plan, err := p.Plan([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(plan); !slices.Equal(got, []string{"app"}) {
		t.Errorf("plan = %v: an installed dependency is not reinstalled, nor are its dependencies", got)
	}
	p.Kegs = []cellar.Keg{keg("app", "1.0")}
	plan, err = p.Plan([]string{"app", "base"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(plan); !slices.Equal(got, []string{"base"}) || !slices.Equal(plan.AlreadyInstalled, []string{"app"}) {
		t.Errorf("plan = %v, already installed = %v", got, plan.AlreadyInstalled)
	}
}

func TestPlanNamedFormulaeAreOnRequestEvenWhenAnotherNeedsThem(t *testing.T) {
	idx := index(t, f("app", "lib"), f("lib"))
	plan, err := planner(t, mac, idx).Plan([]string{"app", "lib", "app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := names(plan); !slices.Equal(got, []string{"lib", "app"}) {
		t.Errorf("plan = %v", got)
	}
	for _, it := range plan.Items {
		if !it.OnRequest {
			t.Errorf("%s was named but is not on request", it.Formula.Name)
		}
	}
}

func TestPlanResolvesAliases(t *testing.T) {
	idx := index(t, with(f("python@3.14"), "aliases", []string{"python", "python3"}))
	plan, err := planner(t, mac, idx).Plan([]string{"python3"})
	if err != nil || !slices.Equal(names(plan), []string{"python@3.14"}) {
		t.Errorf("plan = %v, %v", plan, err)
	}
}

func TestPlanUnknownFormula(t *testing.T) {
	if _, err := planner(t, mac, index(t, f("a"))).Plan([]string{"nosuch"}); !errors.Is(err, deps.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestPlanPicksTheBottleForTheHost(t *testing.T) {
	idx := index(t, f("a"))
	plan, err := planner(t, linux, idx).Plan([]string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Items[0].Tag != "x86_64_linux" {
		t.Errorf("tag = %v", plan.Items[0].Tag)
	}
	if plan.Items[0].Bottle.SHA256 != "x86_64_linux" {
		t.Errorf("bottle = %+v, want the file for that tag", plan.Items[0].Bottle)
	}
}

func TestPlanPkgVersionIncludesTheRevision(t *testing.T) {
	idx := index(t, with(f("git"), "revision", 1), f("jq"))
	plan, err := planner(t, mac, idx).Plan([]string{"git", "jq"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Items[0].PkgVersion != "1.0_1" || plan.Items[1].PkgVersion != "1.0" {
		t.Errorf("versions = %q, %q", plan.Items[0].PkgVersion, plan.Items[1].PkgVersion)
	}
}

func TestPlanRefusesWhatItCannotInstall(t *testing.T) {
	noBottle := with(f("nobottle"), "bottle", map[string]any{"stable": map[string]any{"files": map[string]any{"x86_64_linux": map[string]any{}}}})
	tests := map[string]struct {
		idx  *api.Index
		want error
		in   string
	}{
		"no bottle for the host": {index(t, noBottle), ErrNoBottle, "nobottle"},
		"disabled":               {index(t, with(f("old"), "disabled", true)), ErrDisabled, "old"},
		"an unsupported step":    {index(t, with(f("pi"), "post_install_steps", []any{map[string]any{"type": "gdk_pixbuf_query_loaders"}})), ErrPostInstall, "pi"},
		"a dependency with one":  {index(t, f("app", "pi"), with(f("pi"), "post_install_steps", []any{map[string]any{"type": "configure_gcc_runtime"}})), ErrPostInstall, "app"},
		"a step run with sudo":   {index(t, with(f("su"), "post_install_steps", []any{map[string]any{"type": "touch", "sudo": true}})), ErrPostInstall, "su"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := planner(t, mac, tt.idx).Plan([]string{tt.in})
			if !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestPlanBottlesBuiltForAnotherPrefix(t *testing.T) {
	idx := index(t, f("a"), f("b"), f("c"))
	p := planner(t, mac, idx)
	p.Bottles = fakeBottles{"a": ":any_skip_relocation", "b": "/opt/homebrew/Cellar", "c": "/usr/local/Cellar"}
	if _, err := p.Plan([]string{"a", "b"}); err != nil {
		t.Errorf("the placeholder cellars and this prefix's own cellar are fine: %v", err)
	}
	if _, err := p.Plan([]string{"c"}); !errors.Is(err, ErrOtherPrefix) {
		t.Errorf("error = %v, want ErrOtherPrefix", err)
	}
	p.Prefix = "/some/where/else"
	if _, err := p.Plan([]string{"b"}); !errors.Is(err, ErrOtherPrefix) {
		t.Errorf("a bottle for /opt/homebrew into /some/where/else: %v, want ErrOtherPrefix", err)
	}
}

func TestPlanLinuxNeedsNoRelocation(t *testing.T) {
	idx := index(t, f("a"), f("b"))
	p := planner(t, linux, idx)
	p.Prefix = "/home/linuxbrew/.linuxbrew"
	p.Bottles = fakeBottles{"a": ":any_skip_relocation", "b": ":any"}
	if _, err := p.Plan([]string{"a"}); err != nil {
		t.Errorf("a bottle that needs no relocation: %v", err)
	}
	if _, err := p.Plan([]string{"b"}); !errors.Is(err, ErrUnsupportedHost) {
		t.Errorf("error = %v, want ErrUnsupportedHost: rewriting ELF files is not done", err)
	}
}

func TestPlanWarnsAboutDeprecatedFormulae(t *testing.T) {
	idx := index(t, with(with(f("old"), "deprecated", true), "deprecation_reason", "unmaintained"))
	plan, err := planner(t, mac, idx).Plan([]string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items[0].Warnings) != 1 || !strings.Contains(plan.Items[0].Warnings[0], "not maintained upstream") {
		t.Errorf("warnings = %v", plan.Items[0].Warnings)
	}
}

func TestPlanRefusesConflictsWithLinkedFormulae(t *testing.T) {
	idx := index(t, with(f("a"), "conflicts_with", []string{"b"}), f("b"))
	p := planner(t, mac, idx)
	p.Kegs = []cellar.Keg{keg("b", "1.0")}
	if _, err := p.Plan([]string{"a"}); err != nil {
		t.Errorf("a conflicting formula that is not linked is no obstacle: %v", err)
	}
	p.Linked = func(name string) bool { return name == "b" }
	if _, err := p.Plan([]string{"a"}); !errors.Is(err, ErrConflict) {
		t.Errorf("error = %v, want ErrConflict", err)
	}
}

func TestPlanRefusesAFormulaInstalledFromAnotherTap(t *testing.T) {
	idx := index(t, f("bun"))
	p := planner(t, mac, idx)
	k := keg("bun", "1.0")
	k.Receipt.Source.Tap = "oven-sh/bun"
	p.Kegs = []cellar.Keg{k}
	if _, err := p.Plan([]string{"bun"}); !errors.Is(err, ErrOtherTap) {
		t.Errorf("error = %v, want ErrOtherTap", err)
	}
}

func TestPlanKegOnlyFollowsThePlatformVariation(t *testing.T) {
	idx := index(t, with(f("tool"), "variations", map[string]any{"arm64_tahoe": map[string]any{"keg_only": true}}))
	plan, err := planner(t, mac, idx).Plan([]string{"tool"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Items[0].KegOnly {
		t.Error("not keg-only on the Mac, where the variation says it is")
	}
	plan, err = planner(t, linux, idx).Plan([]string{"tool"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Items[0].KegOnly {
		t.Error("keg-only on Linux, where there is no such variation")
	}
}

func TestPlanReportsEveryProblem(t *testing.T) {
	idx := index(t, with(f("a"), "disabled", true), with(f("b"), "disabled", true), f("c"))
	_, err := planner(t, mac, idx).Plan([]string{"a", "b", "c"})
	if err == nil || !strings.Contains(err.Error(), "a") || !strings.Contains(err.Error(), "b") || !errors.Is(err, ErrDisabled) {
		t.Errorf("error = %v, want one that names both a and b", err)
	}
}

func TestPlanNothingToDo(t *testing.T) {
	plan, err := planner(t, mac, index(t, f("a"))).Plan(nil)
	if err != nil || len(plan.Items) != 0 {
		t.Errorf("plan = %v, %v", plan, err)
	}
}

func TestPlanAllowsPostInstallStepsItCanRun(t *testing.T) {
	steps := []any{map[string]any{"type": "mkdir_p", "path": map[string]any{"base": "var", "path": "x"}}, map[string]any{"type": "run", "command": map[string]any{"path": "true"}}}
	idx := index(t, with(f("pi"), "post_install_steps", steps))
	if _, err := planner(t, mac, idx).Plan([]string{"pi"}); err != nil {
		t.Errorf("steps brw can run should not stop the install: %v", err)
	}
}

func TestPlanNamesTheUnsupportedSteps(t *testing.T) {
	steps := []any{map[string]any{"type": "touch"}, map[string]any{"type": "gtk_update_icon_cache"}, map[string]any{"type": "gtk_update_icon_cache"}, map[string]any{"type": "configure_php"}}
	_, err := planner(t, mac, index(t, with(f("pi"), "post_install_steps", steps))).Plan([]string{"pi"})
	if err == nil || !strings.Contains(err.Error(), "gtk_update_icon_cache, configure_php") || strings.Contains(err.Error(), "touch") {
		t.Errorf("error = %v, want the unsupported steps once each and not the supported one", err)
	}
}
