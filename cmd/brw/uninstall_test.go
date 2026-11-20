package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/install/installtest"
)

// installed returns an env and prefix with libx and app (which needs libx) installed.
func installed(t *testing.T) (*env, string) {
	t.Helper()
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "libx", Version: "2.0", Files: []installtest.File{file("lib/libx.a", "x", 0)}}, nil, nil)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}, Deps: []string{"libx"}}, []string{"libx"}, nil)
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	e.err.(*bytes.Buffer).Reset()
	return e, prefix
}

func TestUninstallRemovesWhatInstallMade(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	e, prefix := installed(t)
	out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "Uninstalling "+filepath.Join(prefix, "Cellar/app/1.0")+"... (") {
		t.Errorf("output = %q", out)
	}
	for _, p := range []string{"Cellar/app", "bin/app", "opt/app", "var/homebrew/linked/app"} {
		if exists(t, prefix, p) {
			t.Errorf("%s is still there", p)
		}
	}
	if !exists(t, prefix, "Cellar/libx/2.0") {
		t.Error("the dependency went too")
	}
}

func TestUninstallRefusesToBreakADependent(t *testing.T) {
	e, prefix := installed(t)
	_, err := runCmd(t, e, "uninstall", "-prefix", prefix, "libx")
	if err == nil || !strings.Contains(err.Error(), "required by app") || !strings.Contains(err.Error(), "--ignore-dependencies libx") {
		t.Fatalf("err = %v", err)
	}
	if !exists(t, prefix, "Cellar/libx/2.0") {
		t.Error("libx was removed")
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--ignore-dependencies", "libx"); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Cellar/libx") {
		t.Error("--ignore-dependencies did not remove it")
	}
}

func TestUninstallDryRunChangesNothing(t *testing.T) {
	e, prefix := installed(t)
	out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "-n", "app")
	if err != nil || !strings.HasPrefix(out, "Would uninstall ") {
		t.Fatalf("output = %q, err = %v", out, err)
	}
	for _, p := range []string{"Cellar/app/1.0", "bin/app", "opt/app", "var/homebrew/linked/app"} {
		if !exists(t, prefix, p) {
			t.Errorf("%s went", p)
		}
	}
}

func TestUninstallNamesWhatIsNotInstalled(t *testing.T) {
	e, prefix := installed(t)
	_, err := runCmd(t, e, "uninstall", "-prefix", prefix, "nosuchthing")
	if err == nil || !strings.Contains(err.Error(), `no available formula with the name "nosuchthing"`) {
		t.Errorf("unknown name: %v", err)
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	_, err = runCmd(t, e, "uninstall", "-prefix", prefix, "app")
	if err == nil || !strings.Contains(err.Error(), "no such keg: "+filepath.Join(prefix, "Cellar/app")) {
		t.Errorf("known formula, not installed: %v", err)
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no names: %v", err)
	}
}

func TestUninstallWorksOffline(t *testing.T) {
	e, prefix := installed(t)
	e.client = &api.Client{URL: "http://127.0.0.1:1", CacheDir: t.TempDir()} // refuses connections, nothing cached
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Cellar/app") {
		t.Error("the keg stayed")
	}
	_, err := runCmd(t, e, "uninstall", "-prefix", prefix, "gone")
	if err == nil || !strings.Contains(err.Error(), "no such keg") {
		t.Errorf("a missing keg offline: %v", err)
	}
}

func TestUninstallJournalsItsChanges(t *testing.T) {
	e, prefix := installed(t)
	journal := filepath.Join(t.TempDir(), "journal")
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "-journal", journal, "app"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(journal)
	if err != nil || !strings.Contains(string(b), "removekeg Cellar/app/1.0\n") {
		t.Errorf("journal = %q, %v", b, err)
	}
}

func TestUninstallForceRemovesEveryVersion(t *testing.T) {
	for _, flag := range []string{"-f", "--force"} {
		e, prefix := installed(t)
		if err := os.CopyFS(filepath.Join(prefix, "Cellar/app/0.9"), os.DirFS(filepath.Join(prefix, "Cellar/app/1.0"))); err != nil {
			t.Fatal(err)
		}
		out, err := runCmd(t, e, "uninstall", "-prefix", prefix, flag, "app")
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(out, "Uninstalling app... (") || exists(t, prefix, "Cellar/app") {
			t.Errorf("%s: output = %q, tree has Cellar/app = %v", flag, out, exists(t, prefix, "Cellar/app"))
		}
	}
}

func TestUninstallAutoremovesUnneededDependencies(t *testing.T) {
	e, prefix := installed(t)
	out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "==> Autoremoving 1 unneeded formula:\nlibx\n") || exists(t, prefix, "Cellar/libx") {
		t.Errorf("libx should go with app:\n%s", out)
	}
}

func TestUninstallLeavesDependenciesWhenAutoremoveIsOff(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	e, prefix := installed(t)
	out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app")
	if err != nil || strings.Contains(out, "Autoremoving") || !exists(t, prefix, "Cellar/libx/2.0") {
		t.Fatalf("output = %q, err = %v", out, err)
	}
	out, err = runCmd(t, e, "autoremove", "-prefix", prefix, "-n")
	if err != nil || out != "==> Would autoremove 1 unneeded formula:\nlibx\n" || !exists(t, prefix, "Cellar/libx/2.0") {
		t.Fatalf("dry run: %q, %v", out, err)
	}
	out, err = runCmd(t, e, "autoremove", "-prefix", prefix)
	if err != nil || !strings.HasPrefix(out, "==> Autoremoving 1 unneeded formula:\nlibx\nUninstalling ") || exists(t, prefix, "Cellar/libx") {
		t.Fatalf("autoremove: %q, %v", out, err)
	}
	if out, err = runCmd(t, e, "autoremove", "-prefix", prefix); err != nil || out != "" {
		t.Errorf("nothing left to do: %q, %v", out, err)
	}
	if _, err := runCmd(t, e, "autoremove", "-prefix", prefix, "extra"); err == nil {
		t.Error("autoremove takes no names")
	}
}

func TestUninstallDryRunPreviewsTheAutoremove(t *testing.T) {
	e, prefix := installed(t)
	out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "-n", "app")
	if err != nil || !strings.HasSuffix(out, "==> Would autoremove 1 unneeded formula:\nlibx\n") || !exists(t, prefix, "Cellar/libx/2.0") {
		t.Errorf("output = %q, err = %v", out, err)
	}
}

func TestAutoremoveKeepsTheBuildDependenciesOfSourceBuiltFormulae(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "libb", Version: "1.0", Files: []installtest.File{file("lib/libb.a", "b", 0)}}, nil, nil)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}}, nil,
		map[string]any{"build_dependencies": []string{"libb"}})
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "libb", "app"); err != nil {
		t.Fatal(err)
	}
	edit := func(name, old, new string) {
		t.Helper()
		p := filepath.Join(prefix, "Cellar", name, "1.0", "INSTALL_RECEIPT.json")
		b, err := os.ReadFile(p)
		if err != nil || !strings.Contains(string(b), old) {
			t.Fatalf("%s: %v", name, err)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(string(b), old, new, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	edit("libb", `"installed_on_request": true`, `"installed_on_request": false`)
	out, _ := runCmd(t, e, "autoremove", "-prefix", prefix, "-n")
	if !strings.Contains(out, "libb") {
		t.Fatalf("a bottled app does not need libb: %q", out)
	}
	edit("app", `"poured_from_bottle": true`, `"poured_from_bottle": false`)
	if out, err := runCmd(t, e, "autoremove", "-prefix", prefix, "-n"); err != nil || out != "" {
		t.Errorf("an app built from source needs libb to build: %q, %v", out, err)
	}
}

// installedCasks returns a store and prefix with the jq formula and the named
// app casks installed into apps.
func installedCasks(t *testing.T, tokens ...string) (*store, *env, string, string) {
	t.Helper()
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	s := newStore(t)
	s.formula(jqBottle(), nil, nil)
	for _, token := range tokens {
		s.appCask(token)
	}
	e, prefix := s.env()
	apps := t.TempDir()
	if out, err := runCmd(t, e, "install", append([]string{"-prefix", prefix, "--appdir", apps, "jq"}, tokens...)...); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	return s, e, prefix, apps
}

func TestUninstallFormulaAndCaskTogether(t *testing.T) {
	_, e, prefix, apps := installedCasks(t, "app")
	out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "jq", "app")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"Uninstalling " + filepath.Join(prefix, "Cellar/jq/1.0"), "==> Uninstalling Cask app\n", "==> Purging files for version 1.0 of Cask app\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output is missing %q:\n%s", want, out)
		}
	}
	for _, p := range []string{"Cellar/jq", "Caskroom/app", "bin/tool"} {
		if exists(t, prefix, p) {
			t.Errorf("%s is still there", p)
		}
	}
	if exists(t, apps, "App.app") {
		t.Error("App.app is still there")
	}
}

func TestUninstallCaskDryRunChangesNothing(t *testing.T) {
	_, e, prefix, apps := installedCasks(t, "app")
	out, err := runCmd(t, e, "uninstall", "-prefix", prefix, "-n", "--cask", "app")
	if err != nil || out != "==> Would uninstall 1 cask:\napp 1.0\n" {
		t.Fatalf("output = %q, err = %v", out, err)
	}
	if !exists(t, prefix, "Caskroom/app/1.0") || !exists(t, apps, "App.app") {
		t.Error("dry run changed something")
	}
}

func TestUninstallCaskThatIsNotInstalled(t *testing.T) {
	_, e, prefix, _ := installedCasks(t)
	_, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--cask", "nosuch")
	if err == nil || err.Error() != "Cask 'nosuch' is not installed." {
		t.Errorf("err = %v", err)
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--cask", "--force", "nosuch"); err != nil {
		t.Errorf("--force: %v", err)
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--cask", "--formula", "jq"); err == nil {
		t.Error("--cask and --formula together should fail")
	}
}

func TestUninstallRefusesTheBrwCask(t *testing.T) {
	_, e, prefix, apps := installedCasks(t, "brw")
	_, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--cask", "brw")
	if err == nil || !strings.Contains(err.Error(), "--force") || !exists(t, prefix, "Caskroom/brw/1.0") {
		t.Fatalf("err = %v", err)
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--cask", "--force", "brw"); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Caskroom/brw") || exists(t, apps, "App.app") {
		t.Error("--force should remove it")
	}
}

func TestUninstallRefusesAPinnedCask(t *testing.T) {
	_, e, prefix, _ := installedCasks(t, "app")
	pins := filepath.Join(prefix, "var/homebrew/pinned_casks")
	if err := os.MkdirAll(pins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Caskroom/app/1.0", filepath.Join(pins, "app")); err != nil {
		t.Fatal(err)
	}
	_, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app")
	if err == nil || err.Error() != "app is pinned. You must unpin it to uninstall." || !exists(t, prefix, "Caskroom/app/1.0") {
		t.Fatalf("err = %v", err)
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--force", "app"); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Caskroom/app") || exists(t, prefix, "var/homebrew/pinned_casks/app") {
		t.Error("--force should unpin and remove it")
	}
}

func TestUninstallRefusesACaskAnotherNeeds(t *testing.T) {
	s, _, prefix, _ := installedCasks(t, "lib")
	// app needs lib; its install is only the record of one.
	s.appCask("app")
	s.casks[1]["depends_on"] = map[string]any{"cask": []string{"lib"}}
	if err := os.MkdirAll(filepath.Join(prefix, "Caskroom/app/.metadata/1.0/20261119103045.123/Casks"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(prefix, "Caskroom/app/.metadata/1.0/20261119103045.123/Casks/app.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	e, _ := s.env()
	_, err := runCmd(t, e, "uninstall", "-prefix", prefix, "lib")
	if err == nil || !strings.Contains(err.Error(), "because it is required by app, which is currently installed.") || !exists(t, prefix, "Caskroom/lib/1.0") {
		t.Fatalf("err = %v", err)
	}
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "--ignore-dependencies", "lib"); err != nil {
		t.Fatal(err)
	}
}
