package uninstall

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// pkg installs a linked keg whose receipt says how it was installed: onRequest
// is "true", "false" or "" for a receipt that does not say, and poured is
// whether it came from a bottle.
func (f *fixture) pkg(name, onRequest string, poured bool, deps ...string) {
	f.t.Helper()
	f.keg(name, "1.0", true, deps...)
	var rd []string
	for _, d := range deps {
		rd = append(rd, `{"full_name":"`+d+`","version":"1","revision":0,"bottle_rebuild":0,"pkg_version":"1","declared_directly":true}`)
	}
	receipt := `{"homebrew_version":"x","poured_from_bottle":` + map[bool]string{true: "true", false: "false"}[poured] + `,`
	if onRequest != "" {
		receipt += `"installed_on_request":` + onRequest + `,`
	}
	receipt += `"runtime_dependencies":[` + strings.Join(rd, ",") + `],"aliases":[],"source":{"tap":"homebrew/core","versions":{"version_scheme":0}}}`
	if err := os.WriteFile(filepath.Join(f.kegPath(name, "1.0"), "INSTALL_RECEIPT.json"), []byte(receipt), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) unneeded() []string {
	f.t.Helper()
	got, err := f.remover().Unneeded()
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func TestUnneededFindsDependenciesNothingNeedsAnyMore(t *testing.T) {
	f := newFixture(t)
	f.pkg("app", "true", true, "libx", "liby")
	f.pkg("libx", "false", true, "libz")
	f.pkg("liby", "false", true)
	f.pkg("libz", "false", true)
	if got := f.unneeded(); len(got) != 0 {
		t.Fatalf("everything is needed, got %v", got)
	}
	if err := f.run(Options{}, "app"); err != nil {
		t.Fatal(err)
	}
	// libx and liby go first, and libz only once libx is known to go.
	if got, want := f.unneeded(), []string{"libx", "liby", "libz"}; !reflect.DeepEqual(sorted(got), want) {
		t.Errorf("Unneeded = %v, want %v", got, want)
	}
}

func sorted(s []string) []string {
	out := append([]string(nil), s...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

func TestUnneededKeepsWhatIsStillNeeded(t *testing.T) {
	f := newFixture(t)
	f.pkg("app", "true", true, "shared")
	f.pkg("other", "true", true, "shared")
	f.pkg("shared", "false", true)
	f.pkg("requested", "true", true) // asked for, needed by nothing
	f.pkg("unsaid", "", true)        // the receipt does not say, so brew leaves it alone
	if got := f.unneeded(); len(got) != 0 {
		t.Errorf("Unneeded = %v", got)
	}
	if err := f.run(Options{}, "app"); err != nil {
		t.Fatal(err)
	}
	if got := f.unneeded(); len(got) != 0 {
		t.Errorf("shared is still needed by other: %v", got)
	}
}

func TestUnneededKeepsWhatSourceBuiltFormulaeNeed(t *testing.T) {
	f := newFixture(t)
	f.pkg("built", "true", false, "libx")    // built from source
	f.pkg("helper", "false", false)          // a dependency built from source is kept
	f.pkg("tool", "false", true, "libbuild") // bottled, but a source build needs libbuild to build
	f.pkg("libbuild", "false", true)
	f.pkg("buildonly", "false", true)
	f.pkg("libx", "false", true)
	r := f.remover()
	r.BuildDeps = func(name string) []string {
		if name == "built" {
			return []string{"buildonly"}
		}
		return nil
	}
	got, err := r.Unneeded()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"tool", "libbuild"}; !reflect.DeepEqual(sorted(got), sorted(want)) {
		t.Errorf("Unneeded = %v, want %v", got, want)
	}
}

func TestUnneededKeepsWhatInstalledCasksNeed(t *testing.T) {
	f := newFixture(t)
	f.pkg("python", "false", true, "mpdecimal")
	f.pkg("mpdecimal", "false", true)
	f.pkg("loose", "false", true)
	f.cask("gcloud-cli", formulaDeps("python", "mpdecimal"))
	if got := f.unneeded(); !reflect.DeepEqual(got, []string{"loose"}) {
		t.Errorf("Unneeded = %v, want [loose]", got)
	}
	f.cask("broken", `{not json`)
	if _, err := f.remover().Unneeded(); err == nil {
		t.Error("a cask receipt that cannot be read should stop autoremove")
	}
}

func TestAutoremoveRemovesAndSaysWhat(t *testing.T) {
	f := newFixture(t)
	f.pkg("libx", "false", true, "liby")
	f.pkg("liby", "false", true)
	f.pkg("keep", "true", true)
	if err := f.remover().Autoremove(); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/libx") || f.exists("Cellar/liby") || !f.exists("Cellar/keep") {
		t.Errorf("tree = %v", f.tree())
	}
	out := f.out.String()
	if !strings.HasPrefix(out, "==> Autoremoving 2 unneeded formulae:\nlibx\nliby\nUninstalling ") {
		t.Errorf("output = %q", out)
	}
	if f.exists("bin/libx") || f.exists("opt/liby") {
		t.Error("links or records stayed")
	}
}

func TestAutoremoveSaysNothingWhenThereIsNothingToDo(t *testing.T) {
	f := newFixture(t)
	f.pkg("keep", "true", true)
	if err := f.remover().Autoremove(); err != nil || f.out.Len() != 0 {
		t.Errorf("output = %q, err = %v", f.out.String(), err)
	}
}

func TestAutoremoveSingularHeading(t *testing.T) {
	f := newFixture(t)
	f.pkg("libx", "false", true)
	if err := f.remover().Autoremove(); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(f.out.String(), "==> Autoremoving 1 unneeded formula:\nlibx\n") {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestAutoremoveDryRunChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.pkg("libx", "false", true)
	before := f.tree()
	r := f.remover()
	r.DryRun = true
	if err := r.Autoremove(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.tree()) {
		t.Error("a dry run changed the prefix")
	}
	if f.out.String() != "==> Would autoremove 1 unneeded formula:\nlibx\n" {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestAutoremoveSkipsAPinnedFormula(t *testing.T) {
	f := newFixture(t)
	f.pkg("libx", "false", true)
	if err := os.MkdirAll(filepath.Join(f.dir, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/libx/1.0", filepath.Join(f.dir, "var/homebrew/pinned/libx")); err != nil {
		t.Fatal(err)
	}
	if err := f.remover().Autoremove(); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/libx/1.0") || !strings.Contains(f.warn.String(), "libx is pinned") {
		t.Errorf("tree = %v, warnings = %q", f.tree(), f.warn.String())
	}
}

func TestUninstallThenAutoremoves(t *testing.T) {
	f := newFixture(t)
	f.pkg("app", "true", true, "libx")
	f.pkg("libx", "false", true)
	if err := f.run(Options{Autoremove: true}, "app"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/app") || f.exists("Cellar/libx") {
		t.Errorf("tree = %v", f.tree())
	}
	if !strings.Contains(f.out.String(), "==> Autoremoving 1 unneeded formula:\nlibx\n") {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestUninstallWithoutAutoremoveLeavesDependencies(t *testing.T) {
	f := newFixture(t)
	f.pkg("app", "true", true, "libx")
	f.pkg("libx", "false", true)
	if err := f.run(Options{}, "app"); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/libx") {
		t.Error("libx went although autoremove was off")
	}
}

func TestUninstallDryRunPreviewsTheAutoremove(t *testing.T) {
	f := newFixture(t)
	f.pkg("app", "true", true, "libx")
	f.pkg("libx", "false", true)
	r := f.remover()
	r.DryRun = true
	if err := r.Run([]string{"app"}, Options{Autoremove: true}); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/app") || !f.exists("Cellar/libx") {
		t.Error("a dry run removed something")
	}
	if !strings.Contains(f.out.String(), "==> Would autoremove 1 unneeded formula:\nlibx\n") {
		t.Errorf("a dry run should preview what the removal would leave unneeded: %q", f.out.String())
	}
}

func TestRefusedUninstallDoesNotAutoremove(t *testing.T) {
	f := newFixture(t)
	f.pkg("app", "true", true, "libx")
	f.pkg("libx", "false", true)
	f.pkg("orphan", "false", true)
	if err := f.run(Options{Autoremove: true}, "libx"); err == nil {
		t.Fatal("libx is needed by app")
	}
	if !f.exists("Cellar/orphan") {
		t.Error("autoremove ran after a refusal")
	}
}

func TestAutoremoveListsNamesSortedAndRemovesInTheOrderTheyBecomeUnneeded(t *testing.T) {
	f := newFixture(t)
	f.pkg("z-top", "false", true, "a-low")
	f.pkg("a-low", "false", true)
	if err := f.remover().Autoremove(); err != nil {
		t.Fatal(err)
	}
	out := f.out.String()
	if !strings.HasPrefix(out, "==> Autoremoving 2 unneeded formulae:\na-low\nz-top\n") {
		t.Errorf("the list should be sorted: %q", out)
	}
	if strings.Index(out, "Cellar/z-top") > strings.Index(out, "Cellar/a-low") {
		t.Errorf("z-top has to go first, a-low is its dependency: %q", out)
	}
}
