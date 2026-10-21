package uninstall

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// cask writes Caskroom/token/.metadata/INSTALL_RECEIPT.json with the given body.
func (f *fixture) cask(token, receipt string) {
	f.t.Helper()
	dir := filepath.Join(f.dir, "Caskroom", token, ".metadata")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "INSTALL_RECEIPT.json"), []byte(receipt), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func formulaDeps(names ...string) string {
	s := `{"runtime_dependencies":{"formula":[`
	for i, n := range names {
		if i > 0 {
			s += ","
		}
		s += `{"full_name":"` + n + `","declared_directly":true}`
	}
	return s + `],"cask":[]}}`
}

func TestCaskDependencies(t *testing.T) {
	f := newFixture(t)
	f.cask("zeta", formulaDeps("python@3.14", "homebrew/core/xz"))
	f.cask("alpha", formulaDeps("python@3.14"))
	f.cask("plain", `{"runtime_dependencies":null}`)
	f.cask("norunt", `{}`)
	if err := os.MkdirAll(filepath.Join(f.dir, "Caskroom", "empty"), 0o755); err != nil { // never installed
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.dir, "Caskroom", ".DS_Store"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(f.dir, "Caskroom", "dangling")); err != nil {
		t.Fatal(err)
	}
	got, err := CaskDependencies(f.dir)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"python@3.14": {"alpha", "zeta"}, "xz": {"zeta"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("CaskDependencies = %v, want %v", got, want)
	}
}

func TestCaskDependenciesWithoutACaskroom(t *testing.T) {
	got, err := CaskDependencies(t.TempDir())
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestCaskDependenciesRefuseToGuess(t *testing.T) {
	f := newFixture(t)
	f.cask("broken", `{not json`)
	if _, err := CaskDependencies(f.dir); err == nil {
		t.Error("an unreadable receipt should be an error")
	}
	g := newFixture(t)
	if err := os.MkdirAll(filepath.Join(g.dir, "Caskroom", "nofile", ".metadata"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := CaskDependencies(g.dir); err == nil {
		t.Error("a cask with metadata but no receipt should be an error")
	}
}

func TestUninstallRefusesWhenAnInstalledCaskNeedsTheKeg(t *testing.T) {
	f := newFixture(t)
	f.keg("python@3.14", "3.14.6", true)
	f.keg("app", "1.0", true, "python@3.14")
	f.cask("gcloud-cli", formulaDeps("python@3.14"))
	err := f.run(Options{}, "python@3.14")
	var de *DependentsError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v", err)
	}
	if want := []string{"app", "gcloud-cli"}; !reflect.DeepEqual(de.Dependents, want) {
		t.Errorf("dependents = %v, want %v", de.Dependents, want)
	}
	if !f.exists("Cellar/python@3.14/3.14.6") {
		t.Error("the keg was removed")
	}
	if err := f.run(Options{IgnoreDependencies: true}, "python@3.14"); err != nil {
		t.Fatal(err)
	}
}

func TestUninstallStopsWhenACaskReceiptCannotBeRead(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	f.cask("broken", `{not json`)
	if err := f.run(Options{}, "jq"); err == nil || !f.exists("Cellar/jq/1.0") {
		t.Errorf("err = %v; the keg should stay when casks cannot be checked", err)
	}
	if err := f.run(Options{IgnoreDependencies: true}, "jq"); err != nil {
		t.Errorf("--ignore-dependencies skips the check: %v", err)
	}
}

func TestUninstallCountsCasksEvenForAKegWithNoOptRecord(t *testing.T) {
	f := newFixture(t)
	f.keg("python@3.14", "3.14.6", false) // not linked, so no opt record
	f.cask("gcloud-cli", formulaDeps("python@3.14"))
	if err := f.run(Options{}, "python@3.14"); err == nil {
		t.Error("brew checks casks against every installed keg, opt-linked or not")
	}
}
