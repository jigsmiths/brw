package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/install/installtest"
)

// makeOutdated turns the installed 1.0 of a formula into a 0.9, which the
// listing says is out of date, keeping it linked if it was.
func makeOutdated(t *testing.T, e *env, prefix, name string) {
	t.Helper()
	wasLinked := exists(t, prefix, "var/homebrew/linked/"+name)
	if wasLinked {
		if _, err := runCmd(t, e, "unlink", "-prefix", prefix, name); err != nil {
			t.Fatal(err)
		}
	}
	versions, err := os.ReadDir(filepath.Join(prefix, "Cellar", name))
	if err != nil || len(versions) != 1 {
		t.Fatalf("%s: %v %v", name, versions, err)
	}
	if err := os.Rename(filepath.Join(prefix, "Cellar", name, versions[0].Name()), filepath.Join(prefix, "Cellar", name, "0.9")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(prefix, "opt", name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/"+name+"/0.9", filepath.Join(prefix, "opt", name)); err != nil {
		t.Fatal(err)
	}
	if wasLinked {
		if _, err := runCmd(t, e, "link", "-prefix", prefix, "--force", name); err != nil {
			t.Fatal(err)
		}
	}
	e.err.(*bytes.Buffer).Reset()
}

func TestUpgradeBringsOutdatedFormulaeUpToDate(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"==> Upgrading app\n  0.9 -> 1.0\n", "==> Pouring app 1.0\n", "==> Running `brw cleanup app`...\nRemoving: " + filepath.Join(prefix, "Cellar/app/0.9") + "... ("} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin/app")); got != "../Cellar/app/1.0/bin/app" {
		t.Errorf("bin/app -> %q", got)
	}
	if exists(t, prefix, "Cellar/app/0.9") || !exists(t, prefix, "Cellar/app/1.0") {
		t.Error("the old keg should be gone and the new one there")
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "opt/app")); got != "../Cellar/app/1.0" {
		t.Errorf("opt/app -> %q", got)
	}
}

func TestUpgradeSaysSoWhenEverythingIsCurrent(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil || out != "Nothing to upgrade.\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestUpgradeOfANamedFormulaThatIsCurrent(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "app")
	if err != nil || out != "" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "app 1.0 already installed") {
		t.Errorf("stderr = %q", w)
	}
}

func TestUpgradeOnlyTheNamedFormulae(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	makeOutdated(t, e, prefix, "versioned")
	if _, err := runCmd(t, e, "upgrade", "-prefix", prefix, "ALIAS-NOT-THERE"); err == nil {
		t.Fatal("an unknown name should fail")
	}
	if exists(t, prefix, "Cellar/app/1.0") {
		t.Fatal("a bad name must not upgrade anything")
	}
	if _, err := runCmd(t, e, "upgrade", "-prefix", prefix, "versioned"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, prefix, "Cellar/versioned/1.0") || exists(t, prefix, "Cellar/app/1.0") {
		t.Error("only versioned should have been upgraded")
	}
}

func TestUpgradeDryRunChangesNothing(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	before := tree(t, prefix)
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "-n")
	if err != nil || out != "==> Would upgrade 1 formula:\napp 0.9 -> 1.0\n" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if !equal(before, tree(t, prefix)) {
		t.Error("a dry run changed the prefix")
	}
}

func TestUpgradeSkipsPinnedFormulaeAndRefusesWhenNamed(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	if err := os.MkdirAll(filepath.Join(prefix, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/app/0.9", filepath.Join(prefix, "var/homebrew/pinned/app")); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil || out != "Nothing to upgrade.\n" || !exists(t, prefix, "Cellar/app/0.9") {
		t.Fatalf("without names a pinned formula is skipped: %q, %v", out, err)
	}
	_, err = runCmd(t, e, "upgrade", "-prefix", prefix, "app")
	if err == nil || !strings.Contains(err.Error(), "not upgrading 1 pinned package:\napp 0.9") {
		t.Errorf("naming it is an error: %v", err)
	}
	if exists(t, prefix, "Cellar/app/1.0") {
		t.Error("a pinned formula was upgraded")
	}
}

func TestUpgradeKeepsAKegOnlyFormulaKegOnly(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "vault")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if exists(t, prefix, "bin/vault") || !exists(t, prefix, "Cellar/vault/1.0") {
		t.Error("a keg-only formula must not get linked by an upgrade")
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "opt/vault")); got != "../Cellar/vault/1.0" {
		t.Errorf("opt/vault -> %q", got)
	}
	if !strings.Contains(out, "vault is keg-only") {
		t.Errorf("the keg-only notice is missing:\n%s", out)
	}
}

func TestUpgradeKeepsWhatTheReceiptSaidAboutRequests(t *testing.T) {
	e, prefix := installed(t) // app on request, libx a dependency
	makeOutdated(t, e, prefix, "libx")
	if _, err := runCmd(t, e, "upgrade", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(prefix, "Cellar/libx/2.0/INSTALL_RECEIPT.json")) // libx is at 2.0 in this store
	if err != nil || strings.Contains(string(b), `"installed_on_request": true`) {
		t.Errorf("a dependency must stay a dependency: %v", err)
	}
}

func TestUpgradeCleanupCanBeSwitchedOff(t *testing.T) {
	t.Setenv("HOMEBREW_NO_INSTALL_CLEANUP", "1")
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil || strings.Contains(out, "Running") || !exists(t, prefix, "Cellar/app/0.9") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestUpgradeCleanupSkipsFormulaeToLeaveAlone(t *testing.T) {
	t.Setenv("HOMEBREW_NO_CLEANUP_FORMULAE", "app")
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	if _, err := runCmd(t, e, "upgrade", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if !exists(t, prefix, "Cellar/app/0.9") {
		t.Error("the old keg of a formula listed in HOMEBREW_NO_CLEANUP_FORMULAE must stay")
	}
}

func TestUpgradeOneFailureDoesNotStopTheOthers(t *testing.T) {
	s := newStore(t)
	bottle := func(name string) installtest.Bottle {
		return installtest.Bottle{Name: name, Version: "1.0", Files: []installtest.File{file("bin/"+name, name, 0o755)}}
	}
	s.formula(bottle("good"), nil, nil)
	s.formula(bottle("bad"), nil, nil)
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "good", "bad"); err != nil {
		t.Fatal(err)
	}
	makeOutdated(t, e, prefix, "good")
	makeOutdated(t, e, prefix, "bad")
	// The listing now says bad needs a step brw cannot run.
	for _, f := range s.formulae {
		if f["name"] == "bad" {
			f["post_install_steps"] = []any{map[string]any{"type": "configure_gcc_runtime"}}
		}
	}
	e2, _ := s.env()
	_, err := runCmd(t, e2, "upgrade", "-prefix", prefix)
	if err == nil || !strings.Contains(err.Error(), "1 formulae cannot be upgraded") {
		t.Fatalf("err = %v", err)
	}
	if !exists(t, prefix, "Cellar/good/1.0") || exists(t, prefix, "Cellar/bad/1.0") || !exists(t, prefix, "Cellar/bad/0.9") {
		t.Error("good should be upgraded and bad left alone")
	}
	if w := e2.err.(*bytes.Buffer).String(); !strings.Contains(w, "bad") || !strings.Contains(w, "configure_gcc_runtime") {
		t.Errorf("stderr = %q", w)
	}
}

func TestUpgradeInstallsDependenciesTheNewVersionNeeds(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}}, nil, nil)
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	makeOutdated(t, e, prefix, "app")
	s.formula(installtest.Bottle{Name: "newdep", Version: "1.0", Files: []installtest.File{file("lib/libnew.a", "n", 0)}}, nil, nil)
	for _, f := range s.formulae {
		if f["name"] == "app" {
			f["dependencies"] = []string{"newdep"}
		}
	}
	e2, _ := s.env()
	out, err := runCmd(t, e2, "upgrade", "-prefix", prefix)
	if err != nil || !exists(t, prefix, "Cellar/newdep/1.0") || !exists(t, prefix, "Cellar/app/1.0") {
		t.Fatalf("%v\n%s", err, out)
	}
	if p, u := strings.Index(out, "==> Pouring newdep 1.0"), strings.Index(out, "==> Upgrading app"); p < 0 || u < 0 || p > u {
		t.Errorf("the new dependency is poured before the upgrade starts:\n%s", out)
	}
}

func TestUpgradeJournalsItsChanges(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	journal := filepath.Join(t.TempDir(), "journal")
	if _, err := runCmd(t, e, "upgrade", "-prefix", prefix, "-journal", journal); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(journal)
	if err != nil || !strings.Contains(string(b), "removekeg Cellar/app/0.9\n") || !strings.Contains(string(b), "rename Cellar/.brw-staging/") {
		t.Errorf("journal = %q, %v", b, err)
	}
}

func TestUpgradeKeepsGoingWhenOneFailsWhilePouring(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	makeOutdated(t, e, prefix, "versioned")
	// The place of the new keg of app is taken, so pouring it fails.
	if err := os.MkdirAll(filepath.Join(prefix, "Cellar/app/1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err == nil || !strings.Contains(err.Error(), "1 of 2 formulae could not be installed") {
		t.Fatalf("err = %v", err)
	}
	if !exists(t, prefix, "Cellar/versioned/1.0") {
		t.Error("the other formula should still be upgraded")
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin/app")); got != "../Cellar/app/0.9/bin/app" {
		t.Errorf("bin/app -> %q: the failed upgrade leaves the old keg linked", got)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "app:") {
		t.Errorf("stderr = %q", w)
	}
}

func TestUpgradeDryRunAlsoListsNewDependencies(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}}, nil, nil)
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	makeOutdated(t, e, prefix, "app")
	s.formula(installtest.Bottle{Name: "newdep", Version: "2.0", Files: []installtest.File{file("lib/libnew.a", "n", 0)}}, nil, nil)
	for _, f := range s.formulae {
		if f["name"] == "app" {
			f["dependencies"] = []string{"newdep"}
		}
	}
	e2, _ := s.env()
	out, err := runCmd(t, e2, "upgrade", "-prefix", prefix, "-n")
	if want := "==> Would upgrade 1 formula:\napp 0.9 -> 1.0\n==> Would install 1 formula:\nnewdep 2.0\n"; err != nil || out != want {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestUpgradeNamedTwiceUpgradesOnce(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "app", "APP", "app")
	if err != nil || strings.Count(out, "==> Upgrading app") != 1 {
		t.Errorf("out = %q, err = %v", out, err)
	}
	out, err = runCmd(t, e, "upgrade", "-prefix", prefix, "-n", "versioned", "versioned")
	if err != nil || strings.Contains(out, "Would upgrade") {
		t.Errorf("a current formula named twice: %q, %v", out, err)
	}
}

func TestUpgradeWarnsAboutDeprecatedFormulae(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "old", Version: "1.0", Files: []installtest.File{file("bin/old", "old", 0o755)}}, nil, nil)
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "old"); err != nil {
		t.Fatal(err)
	}
	makeOutdated(t, e, prefix, "old")
	for _, f := range s.formulae {
		f["deprecated"] = true
		f["deprecation_reason"] = "unmaintained"
	}
	e2, _ := s.env()
	if _, err := runCmd(t, e2, "upgrade", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if w := e2.err.(*bytes.Buffer).String(); !strings.Contains(w, "old is deprecated because it is not maintained upstream") {
		t.Errorf("stderr = %q", w)
	}
}
