package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/install/installtest"
)

func TestReinstallPoursTheFormulaAgain(t *testing.T) {
	e, prefix := linkStore(t)
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/app/1.0/bin/app"), []byte("damaged"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "reinstall", "-prefix", prefix, "app")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{"==> Reinstalling app\n", "==> Pouring app 1.0\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "Cellar/app/1.0/bin/app")); string(b) != "app" {
		t.Errorf("the keg was not poured afresh: %q", b)
	}
	if exists(t, prefix, "Cellar/app/1.0.reinstall") {
		t.Error("the backup is left")
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin/app")); got != "../Cellar/app/1.0/bin/app" {
		t.Errorf("bin/app -> %q", got)
	}
}

func TestReinstallOfAnOutdatedFormulaInstallsTheCurrentVersion(t *testing.T) {
	e, prefix := linkStore(t)
	makeOutdated(t, e, prefix, "app")
	addVersion(t, prefix, "app", "0.9", "0.5") // an older keg still, for cleanup
	out, err := runCmd(t, e, "reinstall", "-prefix", prefix, "app")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !exists(t, prefix, "Cellar/app/1.0") || exists(t, prefix, "Cellar/app/0.9") || exists(t, prefix, "Cellar/app/0.5") {
		t.Errorf("1.0 should be in, and the old kegs gone: %v", tree(t, filepath.Join(prefix, "Cellar")))
	}
	if !strings.Contains(out, "==> Running `brw cleanup app`...\nRemoving: "+filepath.Join(prefix, "Cellar/app/0.5")) {
		t.Errorf("cleanup runs for the other old kegs:\n%s", out)
	}
}

func TestReinstallOfAFormulaThatIsNotInstalledInstallsIt(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "fresh", Version: "1.0", Files: []installtest.File{file("bin/fresh", "f", 0o755)}}, nil, nil)
	e, prefix := s.env()
	out, err := runCmd(t, e, "reinstall", "-prefix", prefix, "fresh")
	if err != nil || !strings.Contains(out, "==> Pouring fresh 1.0") || strings.Contains(out, "Reinstalling") {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if !exists(t, prefix, "bin/fresh") {
		t.Error("it should be installed and linked")
	}
}

func TestReinstallKeepsTheLinkedStateAndWhatWasAskedFor(t *testing.T) {
	e, prefix := linkStore(t)
	if _, err := runCmd(t, e, "reinstall", "-prefix", prefix, "vault", "app"); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "bin/vault") {
		t.Error("a keg-only formula stays unlinked")
	}
	e2, prefix2 := installed(t)
	if _, err := runCmd(t, e2, "reinstall", "-prefix", prefix2, "libx"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(prefix2, "Cellar/libx/2.0/INSTALL_RECEIPT.json"))
	if err != nil || strings.Contains(string(b), `"installed_on_request": true`) {
		t.Errorf("a dependency stays a dependency: %v", err)
	}
}

func TestReinstallIgnoresAPin(t *testing.T) {
	e, prefix := linkStore(t)
	if err := os.MkdirAll(filepath.Join(prefix, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/app/1.0", filepath.Join(prefix, "var/homebrew/pinned/app")); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "reinstall", "-prefix", prefix, "app")
	if err != nil || !strings.Contains(out, "==> Reinstalling app") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestReinstallDryRunChangesNothing(t *testing.T) {
	e, prefix := linkStore(t)
	before := tree(t, prefix)
	out, err := runCmd(t, e, "reinstall", "-prefix", prefix, "-n", "app", "vault")
	if want := "==> Would reinstall 2 formulae:\nvault 1.0\napp 1.0\n"; err != nil || out != want {
		t.Errorf("out = %q, err = %v, want %q", out, err, want)
	}
	if !equal(before, tree(t, prefix)) {
		t.Error("a dry run changed the prefix")
	}
}

func TestReinstallNeedsNamesAndKnowsThem(t *testing.T) {
	e, prefix := linkStore(t)
	if _, err := runCmd(t, e, "reinstall", "-prefix", prefix); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no names: %v", err)
	}
	before := tree(t, prefix)
	_, err := runCmd(t, e, "reinstall", "-prefix", prefix, "app", "nosuchthing")
	if err == nil || !strings.Contains(err.Error(), `no available formula with the name "nosuchthing"`) || !equal(before, tree(t, prefix)) {
		t.Errorf("a bad name must stop everything: %v", err)
	}
}

func TestReinstallOneFailureDoesNotStopTheOthers(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(prefix, "Cellar/good/1.0/bin/good"), []byte("damaged"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range s.formulae {
		if f["name"] == "bad" {
			f["post_install_steps"] = []any{map[string]any{"type": "configure_gcc_runtime"}}
		}
	}
	e2, _ := s.env()
	_, err := runCmd(t, e2, "reinstall", "-prefix", prefix, "good", "bad")
	if err == nil || !strings.Contains(err.Error(), "1 formulae cannot be reinstalled") {
		t.Fatalf("err = %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "Cellar/good/1.0/bin/good")); string(b) != "good" {
		t.Error("good should be reinstalled")
	}
	if !exists(t, prefix, "Cellar/bad/1.0/bin/bad") || exists(t, prefix, "Cellar/bad/1.0.reinstall") {
		t.Error("bad is left exactly as it was")
	}
	if w := e2.err.(*bytes.Buffer).String(); !strings.Contains(w, "configure_gcc_runtime") {
		t.Errorf("stderr = %q", w)
	}
}

func TestReinstallJournalsItsChanges(t *testing.T) {
	e, prefix := linkStore(t)
	journal := filepath.Join(t.TempDir(), "journal")
	if _, err := runCmd(t, e, "reinstall", "-prefix", prefix, "-journal", journal, "app"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(journal)
	if err != nil || !strings.Contains(string(b), "rename Cellar/app/1.0 Cellar/app/1.0.reinstall\n") || !strings.Contains(string(b), "removekeg Cellar/app/1.0.reinstall\n") {
		t.Errorf("journal = %q, %v", b, err)
	}
}

func TestReinstallNamedTwiceReinstallsOnce(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "reinstall", "-prefix", prefix, "app", "APP", "app")
	if err != nil || strings.Count(out, "==> Reinstalling app") != 1 {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestReinstallAcceptsTheNamesOtherCommandsDo(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "reinstall", "-prefix", prefix, "Homebrew/Core/App")
	if err != nil || !strings.Contains(out, "==> Reinstalling app") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}
