package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/tap"
)

// goreleaserTool is a GoReleaser-style formula: a release archive holding
// the program, which it installs and asks for its completions.
func goreleaserTool(t *testing.T, reg *installtest.Registry, version string) string {
	t.Helper()
	dir := "tool-" + version + "/"
	url, sum := reg.AddSource("tool-"+version+".tar.gz", installtest.Tarball(t, map[string]string{
		dir + "tool":        "#!/bin/sh\necho \"$1 $2 " + version + "\"\n",
		dir + "README.md":   "read me",
		dir + "LICENSE":     "licence",
		dir + "config.yaml": "not a metafile",
	}))
	return "class Tool < Formula\n  desc \"A tool from a tap\"\n  homepage \"https://example.com\"\n" +
		"  url \"" + url + "\"\n  sha256 \"" + sum + "\"\n  license \"MIT\"\n\n" +
		"  bottle do\n    sha256 cellar: :any_skip_relocation, x86_64_linux: \"" + zeros + "\"\n  end\n\n" +
		"  depends_on \"app\"\n\n" +
		"  def install\n    bin.install \"tool\"\n    generate_completions_from_executable(bin/\"tool\", \"completion\")\n  end\nend\n"
}

func TestInstallATapFormulaFromItsDownload(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	writeTapFormula(t, origin, "tool", goreleaserTool(t, reg, "1.0"))
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	keg := filepath.Join(prefix, "Cellar/tool/1.0")
	var got []string
	_ = filepath.WalkDir(keg, func(p string, d os.DirEntry, err error) error {
		if err == nil && p != keg {
			rel, _ := filepath.Rel(keg, p)
			fi, _ := os.Lstat(p)
			got = append(got, fi.Mode().String()+" "+rel)
		}
		return nil
	})
	want := []string{
		"drwxr-xr-x .brew", "-rw-r--r-- .brew/tool.rb", "-rw-r--r-- INSTALL_RECEIPT.json", "-rw-r--r-- LICENSE",
		"-rw-r--r-- README.md", "drwxr-xr-x bin", "-r-xr-xr-x bin/tool", "drwxr-xr-x etc", "drwxr-xr-x etc/bash_completion.d",
		"-rw-r--r-- etc/bash_completion.d/tool", "-rw-r--r-- sbom.spdx.json", "drwxr-xr-x share", "drwxr-xr-x share/fish",
		"drwxr-xr-x share/fish/vendor_completions.d", "-rw-r--r-- share/fish/vendor_completions.d/tool.fish",
		"drwxr-xr-x share/zsh", "drwxr-xr-x share/zsh/site-functions", "-rw-r--r-- share/zsh/site-functions/_tool",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("keg:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "share/zsh/site-functions/_tool")); string(b) != "completion zsh 1.0\n" {
		t.Errorf("zsh completion = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(keg, ".brew/tool.rb")); strings.Contains(string(b), "bottle do") {
		t.Errorf("the kept formula has its bottle block:\n%s", b)
	}
	var r map[string]any
	b, _ := os.ReadFile(filepath.Join(keg, "INSTALL_RECEIPT.json"))
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	src := r["source"].(map[string]any)
	if r["poured_from_bottle"] != false || r["built_as_bottle"] != false || r["changed_files"] != nil ||
		src["path"] != filepath.Join(tap.Dir(prefix, "u/t"), "Formula", "tool.rb") || src["tap"] != "u/t" ||
		r["source_modified_time"] != float64(1791806400) { // the archive's newest file

		t.Errorf("receipt: %s", b)
	}
	if deps, _ := r["runtime_dependencies"].([]any); len(deps) != 1 {
		t.Errorf("runtime_dependencies = %v", deps)
	}
	if target, _ := os.Readlink(filepath.Join(prefix, "bin/tool")); target != "../Cellar/tool/1.0/bin/tool" {
		t.Errorf("bin/tool -> %q", target)
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "sbom.spdx.json")); !strings.Contains(string(b), `"referenceLocator": "pkg:brew/u/t/tool@1.0"`) {
		t.Errorf("sbom:\n%s", b)
	}
	if exists(t, prefix, "Cellar/.brw-staging") {
		t.Error("the build directory was left behind")
	}
}

func TestUpgradeATapFormulaFromItsDownload(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	writeTapFormula(t, origin, "tool", goreleaserTool(t, reg, "1.0"))
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatal(err)
	}
	writeTapFormula(t, origin, "tool", goreleaserTool(t, reg, "2.0"))
	if _, err := tap.Pull(t.Context(), tap.Tap{Name: "u/t", Dir: tap.Dir(prefix, "u/t")}); err != nil {
		t.Fatal(err)
	}
	if out, err := runCmd(t, e, "upgrade", "-prefix", prefix); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "share/zsh/site-functions/_tool")); string(b) != "completion zsh 2.0\n" {
		t.Errorf("zsh completion = %q", b)
	}
	if exists(t, prefix, "Cellar/tool/1.0") || !exists(t, prefix, "Cellar/tool/2.0") {
		t.Error("the upgrade should replace 1.0 with 2.0")
	}
}

// As brew's fix_dynamic_linkage does, an absolute symlink into the prefix
// becomes relative, so the keg keeps working wherever it is.
func TestABuildMakesAbsoluteSymlinksRelative(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	writeTapFormula(t, origin, "tool", strings.Replace(goreleaserTool(t, reg, "1.0"), `bin.install "tool"`,
		`bin.install "tool"`+"\n    system \"ln\", \"-s\", (bin/\"tool\").to_s, (bin/\"tl\").to_s\n    system \"ln\", \"-s\", \"/usr/bin/true\", (bin/\"t\").to_s", 1))
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	for link, want := range map[string]string{"bin/tl": "tool", "bin/t": "/usr/bin/true"} {
		if got, _ := os.Readlink(filepath.Join(prefix, "Cellar/tool/1.0", link)); got != want {
			t.Errorf("%s -> %q, want %q", link, got, want)
		}
	}
}

// skip_clean keeps what the cleaner would change: an empty directory here.
func TestSkipCleanKeepsWhatItNames(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	writeTapFormula(t, origin, "tool", strings.Replace(strings.Replace(goreleaserTool(t, reg, "1.0"),
		`bin.install "tool"`, `bin.install "tool"`+"\n    (prefix/\"kept\").mkpath\n    (prefix/\"gone\").mkpath", 1),
		`depends_on "app"`, "depends_on \"app\"\n  skip_clean \"kept\"", 1))
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	if !exists(t, prefix, "Cellar/tool/1.0/kept") || exists(t, prefix, "Cellar/tool/1.0/gone") {
		t.Error("skip_clean should keep kept and the cleaner remove gone")
	}
}

// A build that fails leaves no keg behind and says why.
func TestAFailedBuildLeavesNothing(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	writeTapFormula(t, origin, "tool", strings.Replace(goreleaserTool(t, reg, "1.0"), `bin.install "tool"`, `bin.install "missing"`, 1))
	_, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool")
	if err == nil {
		t.Fatal("a failed build installed")
	}
	if !strings.Contains(err.Error(), "no such file to install: missing") {
		t.Errorf("err = %v\n%s", err, e.err)
	}
	if exists(t, prefix, "Cellar/tool") || exists(t, prefix, "Cellar/.brw-staging") {
		t.Error("a failed build left files behind")
	}
}
