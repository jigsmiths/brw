package cask

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
)

func (ti *testInstall) keg(t *testing.T, name, version string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(ti.prefix, "Cellar", name, version), 0o755); err != nil {
		t.Fatal(err)
	}
}

func (ti *testInstall) installedCask(t *testing.T, token, version string) {
	t.Helper()
	dir := filepath.Join(ti.prefix, "Caskroom", token, ".metadata", version, "20260101000000.000", "Casks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, token+".json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (ti *testInstall) caskWithDeps(t *testing.T, deps api.CaskDependsOn) api.Cask {
	t.Helper()
	c := ti.cask(t, appArtifacts)
	c.DependsOn = deps
	return c
}

func TestInstallRefusesWhenADependencyIsMissing(t *testing.T) {
	for name, tc := range map[string]struct {
		deps api.CaskDependsOn
		want string
	}{
		"formula": {api.CaskDependsOn{Formula: []string{"jq"}}, "formula jq"},
		"cask":    {api.CaskDependsOn{Cask: []string{"dep"}}, "cask dep"},
	} {
		ti := newTestInstall(t, appTarball)
		err := ti.in.Install(context.Background(), ti.caskWithDeps(t, tc.deps))
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "not installed") {
			t.Errorf("%s: err = %v, want it to name %q as not installed", name, err, tc.want)
		}
		if _, err := os.Lstat(filepath.Join(ti.prefix, "Caskroom/app")); err == nil {
			t.Errorf("%s: the cask was staged", name)
		}
	}
}

func TestInstallSatisfiesDependenciesAfterTheDownloadAndBeforeStaging(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	c := ti.caskWithDeps(t, api.CaskDependsOn{Formula: []string{"jq"}})
	var downloads int32
	var staged bool
	ti.in.SatisfyDependencies = func(_ context.Context, got api.Cask) error {
		downloads = ti.hits.Load()
		_, err := os.Lstat(filepath.Join(ti.prefix, "Caskroom/app"))
		staged = err == nil
		ti.keg(t, "jq", "1.0")
		_, _ = ti.out.WriteString("==> Installing dependencies: jq\n")
		return nil
	}
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if downloads != 1 || staged {
		t.Errorf("at the hook: downloads = %d, staged = %v; want 1 and false", downloads, staged)
	}
	out := ti.out.String()
	if i, j := strings.Index(out, "==> Installing dependencies: jq"), strings.Index(out, "==> Installing Cask app"); i < 0 || j < i {
		t.Errorf("dependencies should come before the cask:\n%s", out)
	}
}

func TestInstallAsDependencyDoesNotSatisfyDependencies(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	ti.in.SatisfyDependencies = func(context.Context, api.Cask) error {
		t.Error("a dependency's own dependencies are already in the top-level cask's list")
		return nil
	}
	ti.keg(t, "jq", "1.0")
	if err := ti.in.InstallAsDependency(context.Background(), ti.caskWithDeps(t, api.CaskDependsOn{Formula: []string{"jq"}})); err != nil {
		t.Fatal(err)
	}
}

func TestInstallStopsWhenDependenciesFail(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	ti.in.SatisfyDependencies = func(context.Context, api.Cask) error { return errors.New("jq failed") }
	err := ti.in.Install(context.Background(), ti.caskWithDeps(t, api.CaskDependsOn{Formula: []string{"jq"}}))
	if err == nil || !strings.Contains(err.Error(), "jq failed") {
		t.Errorf("err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(ti.prefix, "Caskroom/app")); err == nil {
		t.Error("the cask was staged")
	}
}

func TestUpgradeSatisfiesDependenciesBeforeTouchingTheOldVersion(t *testing.T) {
	ti, _, c := upgradable(t, appArtifacts)
	c.DependsOn = api.CaskDependsOn{Formula: []string{"jq"}}
	app := filepath.Join(ti.appDir, "App.app/Contents/Info.plist")
	var oldAppThere bool
	ti.in.SatisfyDependencies = func(context.Context, api.Cask) error {
		oldAppThere = readFile(t, app) == "<plist/>"
		return errors.New("jq failed")
	}
	u, err := ti.in.PrepareUpgrade(context.Background(), installedCask(t, ti.prefix), ti.in.Where, c)
	if err != nil {
		t.Fatalf("preparing should not need the dependencies yet: %v", err)
	}
	if err := u.Run(context.Background()); err == nil || !strings.Contains(err.Error(), "jq failed") {
		t.Fatalf("err = %v", err)
	}
	if !oldAppThere || readFile(t, app) != "<plist/>" || installedCask(t, ti.prefix).Version != "1.0" {
		t.Error("the old version should be untouched when its replacement's dependencies fail")
	}
}

func TestInstallRecordsDependenciesInTheReceipt(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	ti.keg(t, "jq", "1.7.1_2")
	ti.installedCask(t, "dep", "3.0")
	c := ti.caskWithDeps(t, api.CaskDependsOn{Formula: []string{"homebrew/core/jq"}, Cask: []string{"dep"}})
	ti.in.RuntimeDeps = func(api.Cask) (RuntimeDependencies, error) {
		return RuntimeDependencies{
			Cask:    []CaskDependency{{FullName: "dep", Version: "3.0", DeclaredDirectly: true}},
			Formula: []cellar.Dependency{{FullName: "homebrew/core/jq", Version: "1.7.1", Revision: 2, PkgVersion: "1.7.1_2", DeclaredDirectly: true}},
		}, nil
	}
	if err := ti.in.Install(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"runtime_dependencies": {
    "cask": [
      {
        "full_name": "dep",
        "version": "3.0",
        "declared_directly": true
      }
    ],
    "formula": [
      {
        "full_name": "homebrew/core/jq",
        "version": "1.7.1",
        "revision": 2,
        "bottle_rebuild": 0,
        "pkg_version": "1.7.1_2",
        "declared_directly": true
      }
    ]
  }`,
		`"installed_on_request": true`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("receipt is missing %s:\n%s", want, b)
		}
	}
}

func TestInstallAsDependencyIsNotOnRequest(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	if err := ti.in.InstallAsDependency(context.Background(), ti.cask(t, appArtifacts)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"installed_on_request": false`) {
		t.Errorf("receipt:\n%s", b)
	}
}
