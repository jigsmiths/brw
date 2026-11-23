package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/tap"
)

const zeros = "0000000000000000000000000000000000000000000000000000000000000000"

// toolFormula is a tap formula at version, depending on the API's app.
func toolFormula(class, version string) string {
	return "class " + class + " < Formula\n  desc \"A tool from a tap\"\n  homepage \"https://example.com\"\n" +
		"  url \"https://example.com/tool-" + version + ".tar.gz\"\n  sha256 \"" + zeros + "\"\n\n  depends_on \"app\"\nend\n"
}

func writeTapFormula(t *testing.T, repo, name, rb string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, "Formula", name+".rb"), rb)
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-q", "-m", name)
}

// tapFormulaStore is a prefix with the API's app formula installed and the
// tap u/t, from origin, listing tool 1.0.
func tapFormulaStore(t *testing.T) (e *env, prefix, origin string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "")
	e, prefix = linkStore(t)
	origin = t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	writeTapFormula(t, origin, "tool", toolFormula("Tool", "1.0"))
	if _, err := tap.Clone(t.Context(), prefix, "u/t", origin); err != nil {
		t.Fatal(err)
	}
	e.err = &bytes.Buffer{}
	return e, prefix, origin
}

func trustTap(t *testing.T, e *env, prefix, name string) {
	t.Helper()
	if _, err := runCmd(t, e, "trust", "-prefix", prefix, name); err != nil {
		t.Fatal(err)
	}
}

func TestOutdatedChecksAFormulaAgainstItsTap(t *testing.T) {
	e, prefix, _ := tapFormulaStore(t)
	trustTap(t, e, prefix, "u/t")
	tapKeg(t, prefix, "tool", "0.9", "u/t")
	out, err := runCmd(t, e, "outdated", "-prefix", prefix, "--formula")
	if err != nil || out != "u/t/tool\n" {
		t.Errorf("outdated = %q, %v", out, err)
	}
	if w := e.err.(*bytes.Buffer).String(); w != "" {
		t.Errorf("stderr = %q", w)
	}
	out, _ = runCmd(t, e, "outdated", "-prefix", prefix, "--formula", "--json")
	var doc struct{ Formulae []outdatedJSON }
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Formulae) != 1 ||
		doc.Formulae[0].Name != "u/t/tool" || doc.Formulae[0].CurrentVersion != "1.0" {
		t.Errorf("outdated --json = %s", out)
	}
}

func TestInfoAndDepsOfATapFormula(t *testing.T) {
	e, prefix, _ := tapFormulaStore(t)
	trustTap(t, e, prefix, "u/t")
	for _, name := range []string{"u/t/tool", "tool"} {
		out, err := runCmd(t, e, "info", "-prefix", prefix, name)
		if err != nil || !strings.HasPrefix(out, "==> u/t/tool: stable 1.0") {
			t.Errorf("info %s = %q, %v", name, out, err)
		}
	}
	if out, err := runCmd(t, e, "deps", "-prefix", prefix, "u/t/tool"); err != nil || out != "app\n" {
		t.Errorf("deps = %q, %v", out, err)
	}
	if out, err := runCmd(t, e, "search", "-prefix", prefix, "--formula", "tool"); err != nil || !strings.Contains(out, "u/t/tool") {
		t.Errorf("search = %q, %v", out, err)
	}
}

// As brew info does, a bare name means the formula its installed keg came
// from, which shadows the API's.
func TestInfoOfABareNameShowsTheInstalledTapsFormula(t *testing.T) {
	e, prefix, origin := tapFormulaStore(t)
	writeTapFormula(t, origin, "app", "class App < Formula\n  desc \"The tap's app\"\n  url \"https://example.com/app-2.0.tar.gz\"\nend\n")
	if _, err := tap.Pull(t.Context(), tap.Tap{Name: "u/t", Dir: tap.Dir(prefix, "u/t")}); err != nil {
		t.Fatal(err)
	}
	trustTap(t, e, prefix, "u/t")
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	tapKeg(t, prefix, "app", "2.0", "u/t")
	out, err := runCmd(t, e, "info", "-prefix", prefix, "app")
	if want := "==> app: stable 2.0\nWarning: `app` shadows `homebrew/core/app`.\nThe tap's app\n"; err != nil || !strings.HasPrefix(out, want) {
		t.Errorf("info app = %q, %v; want it to start %q", out, err, want)
	}
	if out, _ := runCmd(t, e, "info", "-prefix", prefix, "u/t/app"); !strings.HasPrefix(out, "==> u/t/app: stable 2.0\nThe tap's app\n") {
		t.Errorf("info u/t/app = %q", out)
	}
}

// brew reads homebrew/core from the API even when it is tapped, and so does brw.
func TestATappedHomebrewCoreIsNotReadAsATap(t *testing.T) {
	e, prefix, _ := tapFormulaStore(t)
	writeFile(t, filepath.Join(tap.Dir(prefix, "homebrew/core"), "Formula", "o", "onlytap.rb"),
		"class Onlytap < Formula\n  url \"https://example.com/onlytap-9.0.tar.gz\"\nend\n")
	if out, err := runCmd(t, e, "info", "-prefix", prefix, "onlytap"); err == nil {
		t.Errorf("read homebrew/core's file: %q", out)
	}
}

func TestInfoSaysWhyItCannotReadATapFormula(t *testing.T) {
	e, prefix, origin := tapFormulaStore(t)
	writeTapFormula(t, origin, "bad", "require_relative \"../lib/x\"\nclass Bad < Formula\nend\n")
	if _, err := tap.Pull(t.Context(), tap.Tap{Name: "u/t", Dir: tap.Dir(prefix, "u/t")}); err != nil {
		t.Fatal(err)
	}
	trustTap(t, e, prefix, "u/t")
	for _, name := range []string{"bad", "u/t/bad"} {
		if _, err := runCmd(t, e, "info", "-prefix", prefix, name); err == nil || !strings.Contains(err.Error(), "u/t/bad: unsupported") {
			t.Errorf("info %s: %v", name, err)
		}
	}
}

func TestFormulaeOfAnUntrustedTapAreSkipped(t *testing.T) {
	e, prefix, origin := tapFormulaStore(t)
	for _, name := range []string{"tool", "u/t/tool"} {
		_, err := runCmd(t, e, "info", "-prefix", prefix, name)
		if err == nil || !strings.Contains(err.Error(), "refusing to load formula u/t/tool from untrusted tap u/t; run `brw trust --formula u/t/tool` or `brw trust u/t` to trust it") {
			t.Errorf("info %s: %v", name, err)
		}
	}
	// brew warns about an untrusted tap only when it lists every formula
	if w := e.err.(*bytes.Buffer).String(); w != "" {
		t.Errorf("stderr = %q", w)
	}
	if _, err := runCmd(t, e, "search", "-prefix", prefix, "--formula", "app"); err != nil {
		t.Fatal(err)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "Skipping u/t because it is not trusted. Run `brw trust u/t` to trust it.") {
		t.Errorf("stderr = %q", w)
	}
	// a tap cloned from elsewhere than GitHub is trusted by its remote, as brew does
	if out, err := runCmd(t, e, "trust", "-prefix", prefix, "u/t/tool"); err != nil || out != "Trusted formula: "+strings.ToLower(origin)+"/tool\n" {
		t.Errorf("trust u/t/tool = %q, %v", out, err)
	}
	if out, err := runCmd(t, e, "info", "-prefix", prefix, "tool"); err != nil || !strings.HasPrefix(out, "==> u/t/tool:") {
		t.Errorf("info of a trusted formula = %q, %v", out, err)
	}
}

func TestTrustSaysWhenANameIsBothAFormulaAndACask(t *testing.T) {
	e, prefix, origin := tapFormulaStore(t)
	writeFile(t, filepath.Join(tap.Dir(prefix, "u/t"), "Casks", "tool.rb"), "cask \"tool\" do\n  version \"1\"\nend\n")
	if _, err := runCmd(t, e, "trust", "-prefix", prefix, "u/t/tool"); err == nil || !strings.Contains(err.Error(), "--formula") {
		t.Errorf("err = %v", err)
	}
	if out, err := runCmd(t, e, "trust", "-prefix", prefix, "--formula", "u/t/tool"); err != nil || out != "Trusted formula: "+strings.ToLower(origin)+"/tool\n" {
		t.Errorf("trust --formula = %q, %v", out, err)
	}
}

func TestABareNameInTwoTapsIsAmbiguous(t *testing.T) {
	e, prefix, origin := tapFormulaStore(t)
	if _, err := tap.Clone(t.Context(), prefix, "v/t", origin); err != nil {
		t.Fatal(err)
	}
	trustTap(t, e, prefix, "u/t")
	trustTap(t, e, prefix, "v/t")
	_, err := runCmd(t, e, "info", "-prefix", prefix, "tool")
	if err == nil || !strings.Contains(err.Error(), "u/t/tool") || !strings.Contains(err.Error(), "v/t/tool") {
		t.Errorf("err = %v", err)
	}
}

func TestOutdatedWarnsAboutAFormulaBrwCannotRead(t *testing.T) {
	e, prefix, origin := tapFormulaStore(t)
	writeTapFormula(t, origin, "bad", "require_relative \"../lib/x\"\nclass Bad < Formula\nend\n")
	if _, err := tap.Pull(t.Context(), tap.Tap{Name: "u/t", Dir: tap.Dir(prefix, "u/t")}); err != nil {
		t.Fatal(err)
	}
	trustTap(t, e, prefix, "u/t")
	tapKeg(t, prefix, "bad", "0.9", "u/t")
	if _, err := runCmd(t, e, "outdated", "-prefix", prefix, "--formula"); err != nil {
		t.Fatal(err)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "brw: warning: not checking bad: u/t/bad: unsupported") {
		t.Errorf("stderr = %q", w)
	}
}
