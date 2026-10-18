package install

import (
	"cmp"
	"encoding/json"
	"errors"
	"reflect"
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

func TestPlanRecordsTheTagOfTheHost(t *testing.T) {
	plan, err := planner(t, mac, index(t, f("app"))).Plan([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[0].HostTag; got != "arm64_tahoe" {
		t.Errorf("host tag = %q", got)
	}
}

func TestPlanRecordsTheArchOfTheHost(t *testing.T) {
	plan, err := planner(t, mac, index(t, f("app"))).Plan([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if got := plan.Items[0].Arch; got != "arm64" {
		t.Errorf("arch = %q", got)
	}
	if got := hostArch(platform.Host{OS: "linux", Arch: "amd64"}); got != "x86_64" {
		t.Errorf("amd64 = %q, want brew's name", got)
	}
}

// Brew writes the runtime dependencies from the formula as it is now, not from the
// bottle, which can be older.
func TestPlanListsRuntimeDependenciesLikeBrew(t *testing.T) {
	lib := with(with(f("lib", "base"), "versions", map[string]any{"stable": "2.1"}), "revision", 1)
	lib["bottle"] = map[string]any{"stable": map[string]any{"rebuild": 3, "files": map[string]any{"arm64_tahoe": map[string]any{}}}}
	idx := index(t, f("app", "lib"), lib, f("base"))
	plan, err := planner(t, mac, idx).Plan([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	var app Item
	for _, it := range plan.Items {
		if it.Formula.Name == "app" {
			app = it
		}
	}
	want := []cellar.Dependency{
		{FullName: "base", Version: "1.0", PkgVersion: "1.0"},
		{FullName: "lib", Version: "2.1", Revision: 1, BottleRebuild: 3, PkgVersion: "2.1_1", DeclaredDirectly: true},
	}
	if !reflect.DeepEqual(app.RuntimeDeps, want) {
		t.Errorf("runtime dependencies = %+v\nwant %+v", app.RuntimeDeps, want)
	}
	if base := plan.Items[0]; base.RuntimeDeps == nil || len(base.RuntimeDeps) != 0 {
		t.Errorf("a formula with no dependencies has %#v, want an empty list", base.RuntimeDeps)
	}
}

func versionedKegOnly(name string) formula {
	return with(with(f(name), "keg_only", true), "keg_only_reason", map[string]any{"reason": ":versioned_formula"})
}

// Brew links a versioned keg-only formula that was asked for by name, as long as
// nothing related to it is installed.
func TestPlanLinksAVersionedKegOnlyFormulaAskedFor(t *testing.T) {
	// The unversioned name is only an alias of the newest version, as for icu4c.
	idx := index(t, versionedKegOnly("icu4c@77"), with(versionedKegOnly("icu4c@78"), "aliases", []string{"icu4c"}), with(f("icu4c@77-full"), "keg_only", false))
	plan, err := planner(t, mac, idx).Plan([]string{"icu4c@77"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Items[0].KegOnly {
		t.Error("icu4c@77 was asked for and has no relatives installed, so brew links it")
	}
}

func TestPlanKeepsAVersionedFormulaKegOnlyWhenARelativeIsInstalled(t *testing.T) {
	idx := index(t, versionedKegOnly("icu4c@77"), versionedKegOnly("icu4c@78"), f("icu4c"), versionedKegOnly("icu4c@77-full"))
	for _, installed := range []string{"icu4c@78", "icu4c", "icu4c@77-full"} {
		p := planner(t, mac, idx)
		p.Kegs = []cellar.Keg{keg(installed, "1.0")}
		plan, err := p.Plan([]string{"icu4c@77"})
		if err != nil {
			t.Fatal(err)
		}
		if !plan.Items[0].KegOnly {
			t.Errorf("%s is installed, so icu4c@77 stays keg-only", installed)
		}
	}
}

func TestPlanKeepsAFullVersionedFormulaKegOnlyWhenItsBaseIsInstalled(t *testing.T) {
	idx := index(t, versionedKegOnly("foo@1-full"), f("foo-full"))
	p := planner(t, mac, idx)
	p.Kegs = []cellar.Keg{keg("foo-full", "1.0")}
	plan, err := p.Plan([]string{"foo@1-full"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Items[0].KegOnly {
		t.Error("foo-full is installed, so foo@1-full stays keg-only")
	}
}

func TestPlanKeepsAnUpgradedVersionedFormulaKegOnly(t *testing.T) {
	idx := index(t, versionedKegOnly("icu4c@77"))
	p := planner(t, mac, idx)
	p.Kegs = []cellar.Keg{keg("icu4c@77", "0.9")}
	plan, errs := p.PlanReinstall([]string{"icu4c@77"})
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	if !plan.Items[0].KegOnly {
		t.Error("a version of it is installed, so it is not linked afresh")
	}
}

func TestPlanKeepsAVersionedFormulaKegOnlyWhenTheBaseIsKegOnlyOrItIsOnlyNeeded(t *testing.T) {
	base := with(with(f("icu4c"), "keg_only", true), "keg_only_reason", map[string]any{"reason": ":provided_by_macos"})
	plan, err := planner(t, mac, index(t, versionedKegOnly("icu4c@77"), base)).Plan([]string{"icu4c@77"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Items[0].KegOnly {
		t.Error("a keg-only unversioned formula keeps icu4c@77 keg-only")
	}
	idx := index(t, f("app", "icu4c@77"), versionedKegOnly("icu4c@77"))
	plan, err = planner(t, mac, idx).Plan([]string{"app"})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Items[0].KegOnly {
		t.Error("a dependency is not asked for, so it stays keg-only")
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
	plan, err := planner(t, mac, idx).Plan([]string{"lib", "app", "lib"})
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

// Brew installs a formula that an earlier name needs as a dependency, so naming it
// later does not make it a request: brew install app lib leaves lib off.
func TestPlanDependencyNamedAfterItsDependentIsNotOnRequest(t *testing.T) {
	idx := index(t, f("app", "lib"), f("lib"))
	plan, err := planner(t, mac, idx).Plan([]string{"app", "lib"})
	if err != nil {
		t.Fatal(err)
	}
	for _, it := range plan.Items {
		if want := it.Formula.Name == "app"; it.OnRequest != want {
			t.Errorf("%s on request = %v, want %v", it.Formula.Name, it.OnRequest, want)
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
	plan, err := planner(t, mac, idx).Plan([]string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Items[0].Tag != "arm64_tahoe" {
		t.Errorf("tag = %v", plan.Items[0].Tag)
	}
	if plan.Items[0].Bottle.SHA256 != "arm64_tahoe" {
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

func TestPlanRefusesLinux(t *testing.T) {
	idx := index(t, f("a"), f("b"))
	p := planner(t, linux, idx)
	p.Bottles = fakeBottles{"a": ":any_skip_relocation", "b": ":any"}
	for _, name := range []string{"a", "b"} {
		if _, err := p.Plan([]string{name}); !errors.Is(err, ErrUnsupportedHost) {
			t.Errorf("%s: error = %v, want ErrUnsupportedHost: the ELF files are not rewritten yet", name, err)
		}
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

// onRequest is a keg whose receipt says it was installed on request.
func onRequest(k cellar.Keg) cellar.Keg {
	k.Receipt.InstalledOnRequest = true
	return k
}

func upgradePlanner(t *testing.T, idx *api.Index, linked bool, kegs ...cellar.Keg) *Planner {
	t.Helper()
	p := planner(t, mac, idx)
	p.Kegs = kegs
	p.Linked = func(string) bool { return linked }
	return p
}

func TestPlanUpgradeCarriesOverHowTheOldKegWasInstalled(t *testing.T) {
	idx := index(t, f("asked"), f("needed"))
	p := upgradePlanner(t, idx, true, onRequest(keg("asked", "0.9")), keg("needed", "0.9"))
	plan, errs := p.PlanUpgrade([]string{"asked", "needed"})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := names(plan); !slices.Equal(got, []string{"asked", "needed"}) {
		t.Fatalf("plan = %v", got)
	}
	asked, needed := plan.Items[0], plan.Items[1]
	if !asked.OnRequest || needed.OnRequest {
		t.Errorf("on request: asked = %v, needed = %v; each keeps what its old receipt said", asked.OnRequest, needed.OnRequest)
	}
	for _, it := range plan.Items {
		if len(it.Upgrade) != 1 || it.Upgrade[0].Version != "0.9" || it.Linked == nil || !*it.Linked {
			t.Errorf("%s: upgrade = %v, linked = %v", it.Formula.Name, it.Upgrade, it.Linked)
		}
		if it.PkgVersion != "1.0" {
			t.Errorf("%s: version = %s", it.Formula.Name, it.PkgVersion)
		}
	}
}

func TestPlanUpgradeKeepsAnUnlinkedKegUnlinked(t *testing.T) {
	idx := index(t, f("app"))
	plan, errs := upgradePlanner(t, idx, false, keg("app", "0.9")).PlanUpgrade([]string{"app"})
	if len(errs) != 0 || plan.Items[0].Linked == nil || *plan.Items[0].Linked {
		t.Errorf("errs = %v, linked = %v: it was not linked before", errs, plan.Items[0].Linked)
	}
}

func TestPlanUpgradeAFreshInstallHasNothingToCarryOver(t *testing.T) {
	plan, err := planner(t, mac, index(t, f("app"))).Plan([]string{"app"})
	if err != nil || plan.Items[0].Upgrade != nil || plan.Items[0].Linked != nil {
		t.Errorf("a plain install: %+v, %v", plan.Items[0], err)
	}
}

func TestPlanUpgradeInstallsNewDependenciesButLeavesOutdatedOnesAlone(t *testing.T) {
	idx := index(t, f("app", "newdep", "olddep"), f("newdep"), f("olddep"))
	p := upgradePlanner(t, idx, true, onRequest(keg("app", "0.9")), keg("olddep", "0.5"))
	plan, errs := p.PlanUpgrade([]string{"app"})
	if len(errs) != 0 || !slices.Equal(names(plan), []string{"newdep", "app"}) {
		t.Fatalf("plan = %v, errs = %v", names(plan), errs)
	}
	if plan.Items[0].Upgrade != nil || plan.Items[0].OnRequest {
		t.Errorf("a new dependency is installed, as a dependency: %+v", plan.Items[0])
	}
}

func TestPlanUpgradeOrdersDependenciesFirstAndKegOnlyFormulaeBeforeTheRest(t *testing.T) {
	idx := index(t,
		f("app", "lib"), f("lib"), f("plain"),
		with(f("vault"), "keg_only", true),
	)
	p := upgradePlanner(t, idx, true,
		keg("app", "0.9"), keg("lib", "0.9"), keg("plain", "0.9"), keg("vault", "0.9"))
	plan, errs := p.PlanUpgrade([]string{"app", "plain", "vault", "lib"})
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	if got := names(plan); !slices.Equal(got, []string{"vault", "lib", "app", "plain"}) {
		t.Errorf("plan = %v: vault is keg-only so first, lib comes before app", got)
	}
}

func TestPlanUpgradeOneFailureDoesNotStopTheOthers(t *testing.T) {
	noBottle := with(f("nobottle"), "bottle", map[string]any{"stable": map[string]any{"files": map[string]any{"x86_64_linux": map[string]any{}}}})
	idx := index(t, f("good"), noBottle, with(f("pi"), "post_install_steps", []any{map[string]any{"type": "configure_gcc_runtime"}}), f("fine"))
	p := upgradePlanner(t, idx, true, keg("good", "0.9"), keg("nobottle", "0.9"), keg("pi", "0.9"), keg("fine", "0.9"))
	plan, errs := p.PlanUpgrade([]string{"good", "nobottle", "pi", "fine"})
	if got := names(plan); !slices.Equal(got, []string{"good", "fine"}) {
		t.Errorf("plan = %v", got)
	}
	if len(errs) != 2 || !errors.Is(errs[0], ErrNoBottle) || !errors.Is(errs[1], ErrPostInstall) {
		t.Errorf("errors = %v", errs)
	}
}

func TestPlanUpgradeStillRefusesAFormulaFromAnotherTap(t *testing.T) {
	idx := index(t, f("app"))
	other := keg("app", "0.9")
	other.Receipt.Source.Tap = "someone/tap"
	plan, errs := upgradePlanner(t, idx, true, other).PlanUpgrade([]string{"app"})
	if len(plan.Items) != 0 || len(errs) != 1 || !errors.Is(errs[0], ErrOtherTap) {
		t.Errorf("plan = %v, errs = %v", names(plan), errs)
	}
}

func TestPlanUpgradeOfNothing(t *testing.T) {
	plan, errs := upgradePlanner(t, index(t, f("a")), true).PlanUpgrade(nil)
	if len(plan.Items) != 0 || len(errs) != 0 {
		t.Errorf("plan = %v, errs = %v", names(plan), errs)
	}
}

func TestPlanUpgradeDoesNotChangeLaterInstallPlans(t *testing.T) {
	idx := index(t, f("app"))
	p := upgradePlanner(t, idx, true, keg("app", "0.9"))
	if _, errs := p.PlanUpgrade([]string{"app"}); len(errs) != 0 {
		t.Fatal(errs)
	}
	plan, err := p.Plan([]string{"app"})
	if err != nil || len(plan.Items) != 0 || !slices.Equal(plan.AlreadyInstalled, []string{"app"}) {
		t.Errorf("an install after an upgrade plan: %v, %v, %v", names(plan), plan.AlreadyInstalled, err)
	}
}

func TestPlanUpgradeTakesTheReceiptOfTheKegInUse(t *testing.T) {
	idx := index(t, f("app"))
	older := keg("app", "0.8") // not asked for
	newer := onRequest(keg("app", "0.9"))
	plan, errs := upgradePlanner(t, idx, true, older, newer).PlanUpgrade([]string{"app"})
	if len(errs) != 0 || !plan.Items[0].OnRequest || len(plan.Items[0].Upgrade) != 2 {
		t.Errorf("errs = %v, item = %+v: the latest keg says it was asked for", errs, plan.Items[0])
	}
}

func TestPlanUpgradeMergesSharedNewDependenciesOnceWhenOneFormulaFails(t *testing.T) {
	noBottle := with(f("bad"), "bottle", map[string]any{"stable": map[string]any{"files": map[string]any{"x86_64_linux": map[string]any{}}}})
	idx := index(t, f("a", "shared"), f("b", "shared"), f("shared"), noBottle)
	p := upgradePlanner(t, idx, true, keg("a", "0.9"), keg("b", "0.9"), keg("bad", "0.9"))
	plan, errs := p.PlanUpgrade([]string{"a", "bad", "b"})
	if got := names(plan); !slices.Equal(got, []string{"shared", "a", "b"}) || len(errs) != 1 {
		t.Errorf("plan = %v, errs = %v", got, errs)
	}
}

func TestPlanReinstallPlansAFormulaWhoseCurrentVersionIsInstalled(t *testing.T) {
	idx := index(t, f("app"), f("other"))
	p := upgradePlanner(t, idx, true, onRequest(keg("app", "1.0")), keg("other", "1.0"))
	plan, errs := p.PlanReinstall([]string{"app", "other"})
	if len(errs) != 0 || !slices.Equal(names(plan), []string{"app", "other"}) {
		t.Fatalf("plan = %v, errs = %v", names(plan), errs)
	}
	for _, it := range plan.Items {
		if !it.Reinstall || len(it.Upgrade) != 1 || it.Upgrade[0].Version != "1.0" || it.Linked == nil || !*it.Linked {
			t.Errorf("%s: %+v", it.Formula.Name, it)
		}
	}
	if !plan.Items[0].OnRequest || plan.Items[1].OnRequest {
		t.Errorf("each keeps what its receipt said: %v, %v", plan.Items[0].OnRequest, plan.Items[1].OnRequest)
	}
}

func TestPlanReinstallOfAnOutdatedFormulaIsAnUpgradeToo(t *testing.T) {
	plan, errs := upgradePlanner(t, index(t, f("app")), false, keg("app", "0.9")).PlanReinstall([]string{"app"})
	if len(errs) != 0 || plan.Items[0].PkgVersion != "1.0" || !plan.Items[0].Reinstall || plan.Items[0].Upgrade[0].Version != "0.9" {
		t.Fatalf("errs = %v, item = %+v", errs, plan.Items[0])
	}
	if plan.Items[0].Linked == nil || *plan.Items[0].Linked {
		t.Error("it was not linked, so it stays unlinked")
	}
}

func TestPlanReinstallOfAFormulaThatIsNotInstalledIsAPlainInstall(t *testing.T) {
	plan, errs := upgradePlanner(t, index(t, f("app", "lib"), f("lib")), true).PlanReinstall([]string{"app"})
	if len(errs) != 0 || !slices.Equal(names(plan), []string{"lib", "app"}) {
		t.Fatalf("plan = %v, errs = %v", names(plan), errs)
	}
	for _, it := range plan.Items {
		if it.Reinstall || it.Upgrade != nil || it.Linked != nil {
			t.Errorf("%s has nothing to reinstall: %+v", it.Formula.Name, it)
		}
	}
	if !plan.Items[1].OnRequest || plan.Items[0].OnRequest {
		t.Error("the named formula is on request, its dependency is not")
	}
}

func TestPlanReinstallKeepsOrderAndReportsFailuresOneByOne(t *testing.T) {
	noBottle := with(f("bad"), "bottle", map[string]any{"stable": map[string]any{"files": map[string]any{"x86_64_linux": map[string]any{}}}})
	idx := index(t, f("app", "lib"), f("lib"), with(f("vault"), "keg_only", true), noBottle)
	p := upgradePlanner(t, idx, true, keg("app", "1.0"), keg("lib", "1.0"), keg("vault", "1.0"), keg("bad", "1.0"))
	plan, errs := p.PlanReinstall([]string{"app", "bad", "vault", "lib"})
	if got := names(plan); !slices.Equal(got, []string{"vault", "lib", "app"}) || len(errs) != 1 || !errors.Is(errs[0], ErrNoBottle) {
		t.Errorf("plan = %v, errs = %v", got, errs)
	}
	for _, it := range plan.Items {
		if !it.Reinstall || len(it.Upgrade) != 1 {
			t.Errorf("%s: one failure must not turn the others into plain installs: %+v", it.Formula.Name, it)
		}
	}
}

func TestPlanReinstallStillRefusesAFormulaFromAnotherTap(t *testing.T) {
	other := keg("app", "1.0")
	other.Receipt.Source.Tap = "someone/tap"
	plan, errs := upgradePlanner(t, index(t, f("app")), true, other).PlanReinstall([]string{"app"})
	if len(plan.Items) != 0 || len(errs) != 1 || !errors.Is(errs[0], ErrOtherTap) {
		t.Errorf("plan = %v, errs = %v", names(plan), errs)
	}
}

func TestPlanUpgradeIsNotAReinstall(t *testing.T) {
	plan, errs := upgradePlanner(t, index(t, f("app")), true, keg("app", "0.9")).PlanUpgrade([]string{"app"})
	if len(errs) != 0 || plan.Items[0].Reinstall {
		t.Errorf("an upgrade keeps the old keg until cleanup: %+v, %v", plan.Items[0], errs)
	}
}

func TestPlanLocksTheRuntimeClosureOfEveryItem(t *testing.T) {
	idx := index(t,
		f("app", "lib"), f("lib", "base"), f("base"),
		with(f("other", "base"), "build_dependencies", []string{"cmake"}), f("cmake"))
	p := planner(t, mac, idx)
	p.Kegs = []cellar.Keg{keg("lib", "1.0"), keg("base", "1.0")}
	plan, err := p.Plan([]string{"app", "other"})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"app", "base", "lib", "other"}; !slices.Equal(plan.Locks, want) {
		t.Errorf("locks = %v, want %v: installed dependencies and their own, no build dependencies", plan.Locks, want)
	}
}

func TestPlanUpgradeAndReinstallLockTheirDependencies(t *testing.T) {
	idx := index(t, f("app", "lib"), f("lib", "base"), f("base"))
	p := upgradePlanner(t, idx, true, onRequest(keg("app", "1.0")), keg("lib", "1.0"), keg("base", "1.0"))
	plan, errs := p.PlanReinstall([]string{"app"})
	if len(errs) != 0 || !slices.Equal(plan.Locks, []string{"app", "base", "lib"}) {
		t.Errorf("reinstall locks = %v, errs = %v", plan.Locks, errs)
	}
	p = upgradePlanner(t, idx, false, onRequest(keg("app", "0.9")), keg("lib", "1.0"), keg("base", "1.0"))
	plan, errs = p.PlanUpgrade([]string{"app"})
	if len(errs) != 0 || !slices.Equal(plan.Locks, []string{"app", "base", "lib"}) {
		t.Errorf("upgrade locks = %v, errs = %v", plan.Locks, errs)
	}
}

func TestPlanUpgradeLocksWhatIsLeftWhenOneFormulaFails(t *testing.T) {
	noBottle := with(f("nobottle"), "bottle", map[string]any{"stable": map[string]any{"files": map[string]any{"x86_64_linux": map[string]any{}}}})
	idx := index(t, f("good", "base"), noBottle, f("fine"), f("base"))
	p := upgradePlanner(t, idx, true, keg("good", "0.9"), keg("nobottle", "0.9"), keg("fine", "0.9"), keg("base", "1.0"))
	plan, errs := p.PlanUpgrade([]string{"good", "nobottle", "fine"})
	if len(errs) != 1 || !slices.Equal(plan.Locks, []string{"base", "fine", "good"}) {
		t.Errorf("locks = %v, errs = %v", plan.Locks, errs)
	}
}
