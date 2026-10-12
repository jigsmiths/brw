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
