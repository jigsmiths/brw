package main

import (
	"bytes"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/install/installtest"
)

func depCask(token string, formula, casks []string) api.Cask {
	return api.Cask{Token: token, DependsOn: api.CaskDependsOn{Formula: formula, Cask: casks}}
}

func graphSource(casks map[string]api.Cask, formulae map[string][]string) depSource {
	return depSource{
		cask: func(token string) (api.Cask, error) { return casks[token], nil },
		formulaDeps: func(name string) ([]string, error) {
			return formulae[name], nil
		},
		formula: func(name string) (cellar.Dependency, error) {
			return cellar.Dependency{FullName: name, Version: "1.0", PkgVersion: "1.0"}, nil
		},
	}
}

func nodeNames(nodes []depNode) []string {
	var out []string
	for _, n := range nodes {
		out = append(out, n.name)
	}
	return out
}

func TestTsortPutsDependenciesFirst(t *testing.T) {
	src := graphSource(map[string]api.Cask{"dep": depCask("dep", []string{"x"}, nil)}, map[string][]string{"jq": {"age"}})
	g, err := buildGraph(depCask("app", []string{"jq"}, []string{"dep"}), src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := g.tsort()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"age", "jq", "x", "dep"}; !slices.Equal(nodeNames(got), want) {
		t.Errorf("order = %v, want %v", nodeNames(got), want)
	}
}

func TestTsortNamesACycleAsBrewDoes(t *testing.T) {
	a := depCask("a", nil, []string{"b"})
	src := graphSource(map[string]api.Cask{"b": depCask("b", nil, []string{"c"}), "c": depCask("c", nil, []string{"a"}), "a": a}, nil)
	g, err := buildGraph(a, src)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.tsort(); err == nil || err.Error() != "Cask 'a' includes cyclic dependencies on other Casks: b and c" {
		t.Errorf("err = %v", err)
	}
	self := depCask("s", nil, []string{"s"})
	g, err = buildGraph(self, graphSource(map[string]api.Cask{"s": self}, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := g.tsort(); err == nil || err.Error() != "Cask 's' depends on itself." {
		t.Errorf("err = %v", err)
	}
}

// gcloud-cli's receipt lists python@3.14, its dependencies, then theirs.
func TestRuntimeDepsFollowBrewsGraphOrder(t *testing.T) {
	src := graphSource(nil, map[string][]string{
		"python@3.14": {"mpdecimal", "openssl@3", "sqlite", "xz", "zstd"},
		"openssl@3":   {"ca-certificates"},
		"sqlite":      {"readline"},
		"zstd":        {"lz4"},
	})
	got, err := runtimeDeps(depCask("gcloud", []string{"python@3.14"}, nil), src)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, d := range got.Formula {
		names = append(names, fmt.Sprintf("%s:%t", d.FullName, d.DeclaredDirectly))
	}
	want := []string{"python@3.14:true", "mpdecimal:false", "openssl@3:false", "sqlite:false", "xz:false", "zstd:false",
		"ca-certificates:false", "readline:false", "lz4:false"}
	if !slices.Equal(names, want) {
		t.Errorf("formulae = %v\nwant     %v", names, want)
	}
}

func TestRuntimeDepsIncludeWhatDependencyCasksNeed(t *testing.T) {
	dep := depCask("dep", []string{"x"}, nil)
	src := graphSource(map[string]api.Cask{"dep": dep}, nil)
	got, err := runtimeDeps(depCask("app", []string{"y"}, []string{"dep"}), src)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Cask) != 1 || got.Cask[0].FullName != "dep" || !got.Cask[0].DeclaredDirectly {
		t.Errorf("casks = %+v", got.Cask)
	}
	if len(got.Formula) != 2 || got.Formula[0].FullName != "y" || !got.Formula[0].DeclaredDirectly ||
		got.Formula[1].FullName != "x" || got.Formula[1].DeclaredDirectly {
		t.Errorf("formulae = %+v, want y declared then x not", got.Formula)
	}
}

func (s *store) dependsOn(token string, formula, casks []string) {
	s.t.Helper()
	for _, c := range s.casks {
		if c["token"] == token {
			c["depends_on"] = map[string]any{"formula": formula, "cask": casks}
			return
		}
	}
	s.t.Fatalf("no cask %s", token)
}

func (s *store) retarget(token string) {
	s.t.Helper()
	for _, c := range s.casks {
		if c["token"] == token {
			c["artifacts"] = []any{
				map[string]any{"app": []any{"App.app", map[string]any{"target": token + ".app"}}},
				map[string]any{"binary": []any{"tool", map[string]any{"target": token + "-tool"}}},
			}
			return
		}
	}
	s.t.Fatalf("no cask %s", token)
}

func caskOnRequest(t *testing.T, prefix, token string) bool {
	t.Helper()
	installed, err := caskroom.Installed(prefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range installed {
		if c.Token == token {
			return c.OnRequest
		}
	}
	t.Fatalf("%s is not installed", token)
	return false
}

func TestInstallCaskBringsItsDependencies(t *testing.T) {
	s := newStore(t)
	s.formula(jqBottle(), nil, nil)
	s.appCask("dep")
	s.retarget("dep")
	s.dependsOn("dep", []string{"jq"}, nil)
	s.appCask("app")
	s.retarget("app")
	s.dependsOn("app", nil, []string{"dep"})
	e, prefix := s.env()
	apps := t.TempDir()
	out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "--cask", "app")
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if !exists(t, prefix, "Cellar/jq/1.0") || !exists(t, prefix, "Caskroom/dep/1.0") || !exists(t, prefix, "Caskroom/app/1.0") {
		t.Fatal("the formula, the dependency cask and the cask should all be installed")
	}
	kegs, err := e.kegs(prefix)
	if err != nil || len(kegs) != 1 || kegs[0].Receipt == nil || kegs[0].Receipt.InstalledOnRequest {
		t.Errorf("jq should be installed as a dependency: %+v, %v", kegs, err)
	}
	if caskOnRequest(t, prefix, "dep") || !caskOnRequest(t, prefix, "app") {
		t.Error("only the named cask should be on request")
	}
	marks := []string{"==> Installing dependencies: jq, dep\n", "==> Installing Cask dep\n", "==> Pouring jq 1.0\n", "==> Installing Cask app\n"}
	last := -1
	for _, m := range marks {
		i := strings.Index(out, m)
		if i < last || i < 0 {
			t.Fatalf("%q is missing or out of order in:\n%s", m, out)
		}
		last = i
	}
}

func TestInstallCaskSaysWhenDependenciesAreSatisfied(t *testing.T) {
	s := newStore(t)
	s.formula(jqBottle(), nil, nil)
	s.appCask("app")
	s.dependsOn("app", []string{"jq"}, nil)
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "jq"); err != nil {
		t.Fatal(err)
	}
	e.err = &bytes.Buffer{}
	out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "app")
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if !strings.Contains(out, "All dependencies satisfied.\n") || strings.Contains(out, "Installing dependencies") {
		t.Errorf("out:\n%s", out)
	}
	if w := e.err.(*bytes.Buffer).String(); w != "" {
		t.Errorf("stderr = %q, want nothing about the installed dependency", w)
	}
	kegs, _ := e.kegs(prefix)
	if len(kegs) != 1 || !kegs[0].Receipt.InstalledOnRequest {
		t.Error("an installed formula keeps its own on request flag")
	}
}

func TestInstallCaskRemainsQuietWithoutDependencies(t *testing.T) {
	s := newStore(t)
	s.appCask("app")
	e, prefix := s.env()
	out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "app")
	if err != nil || strings.Contains(out, "dependencies") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestInstallCaskStopsWhenADependencyFails(t *testing.T) {
	s := newStore(t)
	s.appCask("app")
	s.dependsOn("app", []string{"nosuchformula"}, nil)
	e, prefix := s.env()
	_, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "app")
	if err == nil {
		t.Fatal("the cask installed without its dependency")
	}
	if exists(t, prefix, "Caskroom/app") {
		t.Error("the cask was staged")
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "nosuchformula") {
		t.Errorf("stderr = %q, want the missing dependency named", w)
	}
}

func TestInstallCaskDryRunListsDependencies(t *testing.T) {
	s := newStore(t)
	s.formula(jqBottle(), nil, nil)
	s.appCask("dep")
	s.appCask("app")
	s.dependsOn("app", []string{"jq"}, []string{"dep"})
	e, prefix := s.env()
	out, err := runCmd(t, e, "install", "-n", "-prefix", prefix, "--appdir", filepath.Join(t.TempDir(), "Apps"), "app")
	want := "==> Would install 1 cask:\napp 1.0\n==> Would install 2 dependencies for app:\ndep jq\n"
	if err != nil || out != want {
		t.Errorf("out = %q, %v, want %q", out, err, want)
	}
	if entries, _ := filepath.Glob(filepath.Join(prefix, "*")); len(entries) != 0 {
		t.Errorf("a dry run wrote %v", entries)
	}
}

func TestInstallCaskRecordsTransitiveDependencies(t *testing.T) {
	s := newStore(t)
	s.formula(jqBottle(), []string{"age"}, nil)
	s.formula(installtest.Bottle{Name: "age", Version: "1.0", Files: []installtest.File{file("bin/age", "#!/bin/sh\n", 0o755)}}, nil, nil)
	s.appCask("app")
	s.dependsOn("app", []string{"jq"}, nil)
	e, prefix := s.env()
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "--cask", "app"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	r := readJSON(t, filepath.Join(prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	formulae, _ := r["runtime_dependencies"].(map[string]any)["formula"].([]any)
	if len(formulae) != 2 {
		t.Fatalf("runtime_dependencies = %v, want jq and its dependency age", r["runtime_dependencies"])
	}
	jq, age := formulae[0].(map[string]any), formulae[1].(map[string]any)
	if jq["full_name"] != "jq" || jq["declared_directly"] != true || age["full_name"] != "age" || age["declared_directly"] != false {
		t.Errorf("formulae = %v", formulae)
	}
}

func TestInstallCasksOneBadCaskDoesNotStopTheOthers(t *testing.T) {
	s := newStore(t)
	s.appCask("a")
	s.appCask("b")
	s.appCask("good")
	s.retarget("good")
	s.dependsOn("a", nil, []string{"b"})
	s.dependsOn("b", nil, []string{"a"})
	e, prefix := s.env()
	_, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "a", "good")
	if w := e.err.(*bytes.Buffer).String(); err == nil || !strings.Contains(w, "brw: a: Cask 'a' includes cyclic dependencies on other Casks: b\n") {
		t.Errorf("err = %v\nstderr = %s", err, w)
	}
	if !exists(t, prefix, "Caskroom/good/1.0") || exists(t, prefix, "Caskroom/a") {
		t.Error("good should install and a should not")
	}
}

func TestInstallCaskTapsTheTapOfAFormulaDependency(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "")
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	t.Setenv("HOMEBREW_TEMP", t.TempDir())
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}}, nil, nil)
	s.appCask("capp")
	s.dependsOn("capp", []string{"u/t/tool"}, nil)
	e, prefix := s.env()
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	writeTapFormula(t, origin, "tool", bottledTool(t, s.reg, "1.0"))
	e.tapRemote = func(string) string { return origin }
	apps := t.TempDir()
	_, _ = runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "capp")
	if !exists(t, prefix, "Library/Taps/u/homebrew-t") {
		t.Fatalf("the tap of the dependency was not cloned:\n%s", e.err)
	}
	trustTap(t, e, prefix, "u/t")
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "capp"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if !exists(t, prefix, "Cellar/tool/1.0") || !exists(t, prefix, "Caskroom/capp/1.0") {
		t.Error("the dependency and the cask should be installed")
	}
}

func TestInstallCaskTapsTheTapOfACaskDependency(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "")
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	s := newStore(t)
	s.appCask("tapdep")
	dep := s.casks[0]
	s.casks = nil
	s.appCask("capp")
	s.retarget("capp")
	s.dependsOn("capp", nil, []string{"u/t/tapdep"})
	e, prefix := s.env()
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	publishTapCask(t, origin, dep, "1.0")
	e.tapRemote = func(string) string { return origin }
	apps := t.TempDir()
	_, _ = runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "capp")
	if !exists(t, prefix, "Library/Taps/u/homebrew-t") || exists(t, prefix, "Caskroom/capp") {
		t.Fatalf("the dependency's tap should be cloned and nothing installed while it is untrusted:\n%s", e.err)
	}
	trustTap(t, e, prefix, "u/t")
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", apps, "capp"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if !exists(t, prefix, "Caskroom/tapdep/1.0") || caskOnRequest(t, prefix, "tapdep") {
		t.Error("the tap cask should be installed as a dependency")
	}
}

func TestInstallCaskReportsADependencyCycle(t *testing.T) {
	s := newStore(t)
	s.appCask("a")
	s.appCask("b")
	s.dependsOn("a", nil, []string{"b"})
	s.dependsOn("b", nil, []string{"a"})
	e, prefix := s.env()
	_, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "a")
	if w := e.err.(*bytes.Buffer).String(); err == nil || !strings.Contains(w, "Cask 'a' includes cyclic dependencies on other Casks: b") {
		t.Errorf("err = %v, stderr = %q, want the cycle named", err, w)
	}
	if exists(t, prefix, "Caskroom/a") || exists(t, prefix, "Caskroom/b") {
		t.Error("something was installed")
	}
}

func TestUpgradeCaskInstallsDependenciesTheNewVersionNeeds(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	s, _, prefix, apps := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	s.formula(installtest.Bottle{Name: "newdep", Version: "1.0", Files: []installtest.File{file("bin/newdep", "#!/bin/sh\n", 0o755)}}, nil, nil)
	s.dependsOn("app", []string{"newdep"}, nil)
	e, _ := s.env()
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	want := "==> Upgrading app\n  0.9 -> 1.0\n==> Installing dependencies: newdep\n"
	if !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}
	kegs, _ := e.kegs(prefix)
	i := slices.IndexFunc(kegs, func(k cellar.Keg) bool { return k.Name == "newdep" })
	if i < 0 || kegs[i].Receipt.InstalledOnRequest || !exists(t, prefix, "Caskroom/app/1.0") || !exists(t, apps, "App.app") {
		t.Errorf("newdep should be there as a dependency and app upgraded: %+v", kegs)
	}
	r := readJSON(t, filepath.Join(prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if f, _ := r["runtime_dependencies"].(map[string]any)["formula"].([]any); len(f) != 1 {
		t.Errorf("the new receipt lists %v", r["runtime_dependencies"])
	}
}

func TestUpgradeLeavesTheOldVersionWhenADependencyFails(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	s, _, prefix, _ := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	s.dependsOn("app", []string{"nosuchformula"}, nil)
	e, _ := s.env()
	if _, err := runCmd(t, e, "upgrade", "-prefix", prefix); err == nil {
		t.Fatal("upgraded without the dependency")
	}
	if !exists(t, prefix, "Caskroom/app/0.9") || exists(t, prefix, "Caskroom/app/1.0") {
		t.Error("the old version should still be there")
	}
}
