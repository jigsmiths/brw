package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/tap"
)

func bottledTool(t *testing.T, reg *installtest.Registry, version string) string {
	t.Helper()
	file := reg.AddPlain(installtest.Bottle{Name: "tool", Version: version, Deps: []string{"app"},
		Files: []installtest.File{file("bin/tool", "#!/bin/sh\necho "+version+"\n", 0o755)}}, "arm64_tahoe", ":any_skip_relocation")
	return "class Tool < Formula\n  desc \"A tool from a tap\"\n  url \"https://example.com/tool-" + version + ".tar.gz\"\n" +
		"  sha256 \"" + zeros + "\"\n\n  bottle do\n    root_url \"" + reg.PlainRoot() + "\"\n" +
		"    sha256 cellar: :any_skip_relocation, arm64_tahoe: \"" + file.SHA256 + "\"\n  end\n\n  depends_on \"app\"\nend\n"
}

func tapBottleStore(t *testing.T) (e *env, prefix, origin string, reg *installtest.Registry) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "")
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	t.Setenv("HOMEBREW_TEMP", t.TempDir())
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}}, nil, nil)
	e, prefix = s.env()
	origin = t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	writeTapFormula(t, origin, "tool", bottledTool(t, s.reg, "1.0"))
	e.tapRemote = func(name string) string {
		if name != "u/t" {
			return filepath.Join(t.TempDir(), "missing")
		}
		return origin
	}
	return e, prefix, origin, s.reg
}

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Naming a tap's formula taps the tap and trusts the formula; the receipt records the tap.
func TestInstallABottledFormulaByFullName(t *testing.T) {
	e, prefix, _, _ := tapBottleStore(t)
	out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool")
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	w := e.err.(*bytes.Buffer).String()
	for _, want := range []string{"==> Tapping u/t\n", "==> Trusted formula u/t/tool\n"} {
		if !strings.Contains(w, want) {
			t.Errorf("stderr lacks %q:\n%s", want, w)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(prefix, "bin/tool")); string(got) != "#!/bin/sh\necho 1.0\n" {
		t.Errorf("bin/tool = %q", got)
	}
	r := readJSON(t, filepath.Join(prefix, "Cellar/tool/1.0/INSTALL_RECEIPT.json"))
	head := strings.TrimSpace(gitOut(t, tap.Dir(prefix, "u/t"), "rev-parse", "HEAD"))
	src := r["source"].(map[string]any)
	if r["loaded_from_api"] != false || r["loaded_from_internal_api"] != false || r["poured_from_bottle"] != true ||
		src["tap"] != "u/t" || src["tap_git_head"] != head || src["path"] != filepath.Join(tap.Dir(prefix, "u/t"), "Formula", "tool.rb") {
		t.Errorf("receipt: %v", r)
	}
	deps, _ := r["runtime_dependencies"].([]any)
	if len(deps) != 1 || deps[0].(map[string]any)["full_name"] != "app" {
		t.Errorf("runtime_dependencies = %v", deps)
	}
	if !exists(t, prefix, "Cellar/app/1.0") {
		t.Error("the dependency from the API was not installed")
	}
}

// brew runs post_install after linking, in a temporary home; its failure leaves the keg installed.
func TestInstallRunsATapFormulasPostInstall(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	code := strings.Replace(bottledTool(t, reg, "1.0"), "  depends_on \"app\"\n",
		"  depends_on \"app\"\n\n  def post_install\n    (var/\"tool\").mkpath\n"+
			"    (etc/\"tool.conf\").write \"home=#{ENV[\"HOME\"]}\\n\" unless (etc/\"tool.conf\").exist?\n"+
			"    ohai \"Open Tool.app to finish\"\n  end\n", 1)
	writeTapFormula(t, origin, "tool", code)
	out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool")
	if err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	conf, err := os.ReadFile(filepath.Join(prefix, "etc/tool.conf"))
	if err != nil {
		t.Fatal(err)
	}
	home := strings.TrimSuffix(strings.TrimPrefix(string(conf), "home="), "\n")
	if !strings.Contains(filepath.Base(home), "tool-postinstall-") {
		t.Errorf("post_install ran with HOME=%q, want a temporary home", home)
	}
	if _, err := os.Stat(home); err == nil {
		t.Error("the temporary home was left behind")
	}
	if fi, err := os.Stat(filepath.Join(prefix, "var/tool")); err != nil || !fi.IsDir() {
		t.Errorf("var/tool: %v", err)
	}
	if want := "==> Running post_install for tool\n==> Open Tool.app to finish\n"; !strings.Contains(out, want) {
		t.Errorf("output lacks %q:\n%s", want, out)
	}

	writeTapFormula(t, origin, "tool", strings.Replace(code, "(var/\"tool\").mkpath", "raise \"no\"", 1))
	if _, err := tap.Pull(t.Context(), tap.Tap{Name: "u/t", Dir: tap.Dir(prefix, "u/t")}); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "reinstall", "-prefix", prefix, "u/t/tool"); err == nil {
		t.Error("a failed post_install should fail the command")
	}
	if !exists(t, prefix, "Cellar/tool/1.0/bin/tool") {
		t.Error("a failed post_install removed the keg")
	}
}

// As brew's load_formula_or_cask does, a same-named cask beats a formula found only in another tap.
func TestABareNameIsACaskOverAnotherTapsFormula(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("HOMEBREW_NO_REQUIRE_TAP_TRUST", "")
	t.Setenv("HOMEBREW_NO_ENV_HINTS", "1")
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}}, nil, nil)
	s.appCask("tool")
	e, prefix := s.env()
	origin := t.TempDir()
	gitIn(t, origin, "init", "-q", "-b", "main")
	writeTapFormula(t, origin, "tool", bottledTool(t, s.reg, "1.0"))
	if _, err := tap.Clone(t.Context(), prefix, "u/t", origin); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, e, "trust", "-prefix", prefix, "u/t"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	warning := "Treating tool as a cask. For the formula, use u/t/tool or specify the `--formula` flag. To silence this message, use the `--cask` flag."
	for _, args := range [][]string{{"info", "-prefix", prefix, "tool"}, {"install", "-n", "-prefix", prefix, "tool"}} {
		e.err.(*bytes.Buffer).Reset()
		out, err := runCmd(t, e, args[0], args[1:]...)
		if err != nil {
			t.Fatalf("%v: %v\n%s\n%s", args, err, out, e.err)
		}
		if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, warning) {
			t.Errorf("%v: stderr lacks the warning:\n%s", args[0], w)
		}
		if strings.Contains(out, "u/t/tool") || !strings.Contains(out, "tool") {
			t.Errorf("%v took the formula:\n%s", args[0], out)
		}
	}
	e.err.(*bytes.Buffer).Reset()
	if out, err := runCmd(t, e, "info", "-prefix", prefix, "u/t/tool"); err != nil || !strings.Contains(out, "==> u/t/tool:") {
		t.Errorf("the full name is the formula: %v\n%s", err, out)
	}
	if out, err := runCmd(t, e, "info", "--formula", "-prefix", prefix, "tool"); err != nil || !strings.Contains(out, "==> u/t/tool:") ||
		strings.Contains(e.err.(*bytes.Buffer).String(), "Treating") {
		t.Errorf("--formula is the formula, without a warning: %v\n%s\n%s", err, out, e.err)
	}
}

func TestUpgradeABottledTapFormula(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatal(err)
	}
	writeTapFormula(t, origin, "tool", bottledTool(t, reg, "2.0"))
	if _, err := tap.Pull(t.Context(), tap.Tap{Name: "u/t", Dir: tap.Dir(prefix, "u/t")}); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, e, "outdated", "-prefix", prefix, "--formula"); err != nil || out != "u/t/tool\n" {
		t.Errorf("outdated = %q, %v", out, err)
	}
	if out, err := runCmd(t, e, "upgrade", "-prefix", prefix, "tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if got, _ := os.ReadFile(filepath.Join(prefix, "bin/tool")); string(got) != "#!/bin/sh\necho 2.0\n" {
		t.Errorf("bin/tool = %q", got)
	}
	if exists(t, prefix, "Cellar/tool/1.0") {
		t.Error("the old keg is still there")
	}
}

func TestInstallOfAFormulaInAnUntrustedTapByBareNameIsRefused(t *testing.T) {
	e, prefix, origin, _ := tapBottleStore(t)
	if _, err := tap.Clone(t.Context(), prefix, "u/t", origin); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "tool"); err == nil {
		t.Error("installed a formula from an untrusted tap by its bare name")
	}
	if exists(t, prefix, "Cellar/tool") {
		t.Error("tool was installed")
	}
}

// A dependency's untapped tap is cloned; naming the formula does not trust the dependency's tap.
func TestInstallTapsTheTapOfADependency(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	helper := reg.AddPlain(installtest.Bottle{Name: "helper", Version: "1.0",
		Files: []installtest.File{file("bin/helper", "helper", 0o755)}}, "arm64_tahoe", ":any_skip_relocation")
	other := t.TempDir()
	gitIn(t, other, "init", "-q", "-b", "main")
	writeTapFormula(t, other, "helper", "class Helper < Formula\n  url \"https://example.com/helper-1.0.tar.gz\"\n"+
		"  bottle do\n    root_url \""+reg.PlainRoot()+"\"\n    sha256 cellar: :any_skip_relocation, arm64_tahoe: \""+helper.SHA256+"\"\n  end\nend\n")
	writeTapFormula(t, origin, "tool", strings.Replace(bottledTool(t, reg, "1.0"), "depends_on \"app\"", "depends_on \"app\"\n  depends_on \"v/t2/helper\"", 1))
	e.tapRemote = func(name string) string {
		return map[string]string{"u/t": origin, "v/t2": other}[name]
	}
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err == nil || !strings.Contains(err.Error()+e.err.(*bytes.Buffer).String(), "untrusted tap v/t2") {
		t.Errorf("err = %v\n%s", err, e.err)
	}
	if !exists(t, prefix, "Library/Taps/v/homebrew-t2") || exists(t, prefix, "Cellar/tool") {
		t.Fatal("the dependency's tap should be tapped and nothing installed")
	}
	trustTap(t, e, prefix, "v/t2")
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if !exists(t, prefix, "Cellar/helper/1.0/bin/helper") || !exists(t, prefix, "Cellar/tool/1.0") {
		t.Error("tool and its dependency should be installed")
	}
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = tap.GitEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
