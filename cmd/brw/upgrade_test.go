package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/install"
	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/outdated"
	"github.com/jigsmiths/brw/internal/sudo"
)

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

func tapKeg(t *testing.T, prefix, name, version, tap string) {
	t.Helper()
	writeFile(t, filepath.Join(prefix, "Cellar", name, version, "INSTALL_RECEIPT.json"), `{"source":{"tap":"`+tap+`"}}`)
	if err := os.Symlink("../Cellar/"+name+"/"+version, filepath.Join(prefix, "opt", name)); err != nil {
		t.Fatal(err)
	}
}

func otherTapStore(t *testing.T) (*env, string) {
	t.Helper()
	e, prefix := linkStore(t)
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "vault"); err != nil {
		t.Fatal(err)
	}
	tapKeg(t, prefix, "fort", "0.1", "djadmin/tap")
	tapKeg(t, prefix, "vault", "0.9", "hashicorp/tap")
	e.err.(*bytes.Buffer).Reset()
	return e, prefix
}

func TestUpgradeOfANamedFormulaFromAnotherTap(t *testing.T) {
	e, prefix := otherTapStore(t)
	for _, name := range []string{"fort", "vault"} {
		_, err := runCmd(t, e, "upgrade", "-prefix", prefix, name)
		if !errors.Is(err, install.ErrOtherTap) || !strings.HasPrefix(err.Error(), name+": ") {
			t.Errorf("%s: err = %v, want ErrOtherTap", name, err)
		}
	}
	if w := e.err.(*bytes.Buffer).String(); strings.Contains(w, "already installed") {
		t.Errorf("stderr = %q", w)
	}
}

func TestUpgradeAndOutdatedWarnAboutFormulaeFromOtherTaps(t *testing.T) {
	e, prefix := otherTapStore(t)
	const want = "brw: warning: not checking formulae from other taps: fort, vault\n"
	for _, cmd := range []string{"upgrade", "outdated"} {
		e.err.(*bytes.Buffer).Reset()
		if _, err := runCmd(t, e, cmd, "-prefix", prefix); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		if w := e.err.(*bytes.Buffer).String(); w != want {
			t.Errorf("%s: stderr = %q, want %q", cmd, w, want)
		}
	}
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
	e, prefix := installed(t)
	makeOutdated(t, e, prefix, "libx")
	if _, err := runCmd(t, e, "upgrade", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(prefix, "Cellar/libx/2.0/INSTALL_RECEIPT.json"))
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
	for _, f := range s.formulae {
		if f["name"] == "bad" {
			f["post_install_steps"] = []any{map[string]any{"type": "made_up_step"}}
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
	if w := e2.err.(*bytes.Buffer).String(); !strings.Contains(w, "bad") || !strings.Contains(w, "made_up_step") {
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

func makeCaskOutdated(t *testing.T, prefix, token string) {
	t.Helper()
	dir := filepath.Join(prefix, "Caskroom", token)
	for _, d := range []string{dir, filepath.Join(dir, ".metadata")} {
		if err := os.Rename(filepath.Join(d, "1.0"), filepath.Join(d, "0.9")); err != nil {
			t.Fatal(err)
		}
	}
	tool := filepath.Join(prefix, "bin/tool")
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "0.9/tool"), tool); err != nil {
		t.Fatal(err)
	}
}

func TestUpgradeBringsOutdatedCasksUpToDate(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "")
	_, e, prefix, apps := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{
		"Some casks with `auto_updates true` or `version :latest` may still require `--greedy`,\n",
		"Hide these hints with `HOMEBREW_NO_ENV_HINTS=1` (see `man brew`).\n",
		"==> Fetching downloads for: app\n==> Upgrading app\n  0.9 -> 1.0\n",
		"==> Purging files for version 0.9 of Cask app\n",
		"app was successfully upgraded!\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Nothing to upgrade.") || strings.Contains(out, "will now attempt to upgrade casks with `auto_updates true`") ||
		strings.Contains(out, "Upgraded 1") {
		t.Errorf("output:\n%s", out)
	}
	if exists(t, prefix, "Caskroom/app/0.9") || !exists(t, prefix, "Caskroom/app/1.0") {
		t.Error("the old version should be gone and the new one there")
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "Caskroom/app/1.0/App.app")); got != filepath.Join(apps, "App.app") {
		t.Errorf("Caskroom link -> %q, want the app in the folder it was installed to", got)
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin/tool")); got != filepath.Join(prefix, "Caskroom/app/1.0/tool") {
		t.Errorf("bin/tool -> %q", got)
	}
	config, err := os.ReadFile(filepath.Join(prefix, "Caskroom/app/.metadata/config.json"))
	if err != nil || !strings.Contains(string(config), `"explicit":{"appdir":"`+apps+`"}`) {
		t.Errorf("config.json lost the explicit app folder: %s, %v", config, err)
	}

	if out, err := runCmd(t, e, "upgrade", "-prefix", prefix); err != nil || out != "Nothing to upgrade.\n" {
		t.Errorf("upgrade again = %q, %v", out, err)
	}
	if out, err := runCmd(t, e, "outdated", "-prefix", prefix); err != nil || out != "" {
		t.Errorf("outdated after upgrade = %q, %v", out, err)
	}
}

func TestUpgradeOnlyTheNamedCask(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	_, e, prefix, _ := installedCasks(t, "app")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "app")
	if err != nil || out != "" {
		t.Errorf("a current cask: out = %q, err = %v", out, err)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "brw: warning: Not upgrading app, the latest version is already installed\n") {
		t.Errorf("stderr = %q", w)
	}
	makeCaskOutdated(t, prefix, "app")
	out, err = runCmd(t, e, "upgrade", "-prefix", prefix, "--cask", "app")
	if err != nil || !strings.Contains(out, "==> Upgrading app\n  0.9 -> 1.0\n") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestUpgradeVerboseReachesThePkgInstaller(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	t.Setenv("HOMEBREW_NO_SUDO", "")
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	for _, flag := range []string{"-v", "--verbose", ""} {
		s := newStore(t)
		s.pkgCask("pk")
		e, prefix := s.env()
		var runs []sudo.Command
		e.sudoRun = func(_ context.Context, c sudo.Command) (sudo.Result, error) {
			runs = append(runs, c)
			return sudo.Result{}, nil
		}
		if out, err := runCmd(t, e, "install", "-prefix", prefix, "--appdir", t.TempDir(), "pk"); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		for _, d := range []string{"Caskroom/pk", "Caskroom/pk/.metadata"} {
			if err := os.Rename(filepath.Join(prefix, d, "1.0"), filepath.Join(prefix, d, "0.9")); err != nil {
				t.Fatal(err)
			}
		}
		runs = nil
		args := []string{"-prefix", prefix}
		if flag != "" {
			args = append(args, flag)
		}
		if out, err := runCmd(t, e, "upgrade", args...); err != nil {
			t.Fatalf("%s: %v\n%s", flag, err, out)
		}
		verbose := false
		for _, c := range runs {
			if c.Exe == "/usr/sbin/installer" {
				verbose = slices.Contains(c.Args, "-verboseR")
			}
		}
		if want := flag != ""; verbose != want {
			t.Errorf("upgrade %s: -verboseR = %v, want %v", flag, verbose, want)
		}
	}
}

func TestUpgradeAcceptsVerbose(t *testing.T) {
	_, e, prefix, _ := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	for _, flag := range []string{"-v", "--verbose", "-d", "--debug"} {
		if out, err := runCmd(t, e, "upgrade", "-prefix", prefix, flag, "--dry-run"); err != nil || !strings.Contains(out, "app") {
			t.Errorf("%s: out = %q, err = %v", flag, out, err)
		}
	}
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "--verbose", "--cask", "app")
	if err != nil || !strings.Contains(out, "app was successfully upgraded!\n") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestUpgradeFormulaFlagLeavesCasksAlone(t *testing.T) {
	_, e, prefix, _ := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "--formula")
	if err != nil || out != "Nothing to upgrade.\n" || !exists(t, prefix, "Caskroom/app/0.9") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestUpgradeCaskDryRunChangesNothing(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	_, e, prefix, apps := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "-n")
	if err != nil || out != "==> Would upgrade 1 outdated package\napp 0.9 -> 1.0\n" {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if !exists(t, prefix, "Caskroom/app/0.9") || exists(t, prefix, "Caskroom/app/1.0") || !exists(t, apps, "App.app") {
		t.Error("dry run changed something")
	}
}

func TestUpgradeSkipsPinnedCasksAndRefusesWhenNamed(t *testing.T) {
	_, e, prefix, _ := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	pins := filepath.Join(prefix, "var/homebrew/pinned_casks")
	if err := os.MkdirAll(pins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(prefix, "Caskroom/app/0.9"), filepath.Join(pins, "app")); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, e, "upgrade", "-prefix", prefix); err != nil || out != "Nothing to upgrade.\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	_, err := runCmd(t, e, "upgrade", "-prefix", prefix, "app")
	if err == nil || err.Error() != "not upgrading 1 pinned package:\napp 0.9 -> 1.0" {
		t.Errorf("err = %v", err)
	}
	if !exists(t, prefix, "Caskroom/app/0.9") {
		t.Error("upgraded a pinned cask")
	}
}

func TestUpgradeLeavesSelfUpdatingCasksUnlessGreedy(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	s, _, prefix, _ := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	s.casks[0]["auto_updates"] = true
	e2, _ := s.env()
	if out, err := runCmd(t, e2, "upgrade", "-prefix", prefix); err != nil || out != "Nothing to upgrade.\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	out, err := runCmd(t, e2, "upgrade", "-prefix", prefix, "--greedy")
	if err != nil || !strings.Contains(out, "==> Upgrading app\n  0.9 -> 1.0\n") {
		t.Errorf("--greedy: out = %q, err = %v", out, err)
	}
}

func TestUpgradeHintsNameBrwNotHomebrew(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "")
	idx := api.NewCaskIndex([]api.Cask{{Token: "app", AutoUpdates: true}})
	var out bytes.Buffer
	printCaskHints(&env{out: &out}, idx, []outdated.Result{{Name: "app"}}, outdated.CaskOptions{})
	if want := "brw will now attempt to upgrade casks with `auto_updates true`.\n"; !strings.HasPrefix(out.String(), want) {
		t.Errorf("hints = %q, want prefix %q", out.String(), want)
	}
}

func TestUpgradeNamesTheCasksThatFailed(t *testing.T) {
	_, e, prefix, _ := installedCasks(t, "app")
	makeCaskOutdated(t, prefix, "app")
	tool := filepath.Join(prefix, "bin/tool")
	if err := os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tool, []byte("not ours"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err == nil || err.Error() != "1 of 1 casks could not be upgraded: app" {
		t.Fatalf("err = %v", err)
	}
	if !strings.HasSuffix(out, "==> Failed to upgrade 1 package:\napp 0.9 -> 1.0\n") {
		t.Errorf("the failed cask is named in the summary:\n%s", out)
	}
	if w := e.err.(*bytes.Buffer).String(); strings.Count(w, "already a Binary") != 2 || !strings.Contains(w, "could not restore app 0.9: ") {
		t.Errorf("the rollback failure should be labelled, not repeated as is:\n%s", w)
	}
}

func TestUpgradeListsEveryUpgradeBeforeFetchingAny(t *testing.T) {
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	_, e, prefix, _ := installedCasks(t, "app")
	makeOutdated(t, e, prefix, "jq")
	makeCaskOutdated(t, prefix, "app")
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	list := "==> Upgrading 2 outdated packages:\njq   0.9 -> 1.0\napp  0.9 -> 1.0\n"
	if !strings.HasPrefix(out, list) {
		t.Errorf("output does not start with the list of upgrades:\n%s", out)
	}
	if strings.Count(out, "==> Upgrading 2 outdated packages") != 1 {
		t.Errorf("the list appears more than once:\n%s", out)
	}
	if !strings.HasSuffix(out, "==> Upgraded 2 outdated packages\njq   0.9 -> 1.0\napp  0.9 -> 1.0\n") {
		t.Errorf("the summary covers formulae and casks:\n%s", out)
	}
}

func TestUpgradeListsPinnedFormulaeOnTheirOwnLine(t *testing.T) {
	_, e, prefix, _ := installedCasks(t, "app")
	makeOutdated(t, e, prefix, "jq")
	makeCaskOutdated(t, prefix, "app")
	if _, err := runCmd(t, e, "pin", "-prefix", prefix, "jq"); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "upgrade", "-prefix", prefix)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := "==> Upgrading 1 outdated package:\napp 0.9 -> 1.0\n==> Pinned, not upgrading 1 package:\njq 0.9 -> 1.0\n"
	if !strings.HasPrefix(out, want) {
		t.Errorf("output:\n%s\nwant it to start with:\n%s", out, want)
	}
}
