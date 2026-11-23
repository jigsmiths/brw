package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/machoedit"
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
	if r["homebrew_version"] != noRepositoryVersion {
		t.Errorf("homebrew_version = %v, want the prefix's Homebrew version", r["homebrew_version"])
	}
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

func TestABuildStagesResourcesAndAppliesPatches(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	url, sum := reg.AddSource("tool-1.0.tar.gz", installtest.Tarball(t, map[string]string{
		"tool-1.0/a.txt": "one\n", "tool-1.0/b.txt": "two\n",
	}))
	resURL, resSum := reg.AddSource("extra-2.0.tar.gz", installtest.Tarball(t, map[string]string{"extra-2.0/lib.txt": "lib\n"}))
	patchURL, patchSum := reg.AddSource("fix.patch", []byte("--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-one\n+uno\n"))
	writeTapFormula(t, origin, "tool", "class Tool < Formula\n  url \""+url+"\"\n  sha256 \""+sum+"\"\n\n"+
		"  resource \"extra\" do\n    url \""+resURL+"\"\n    sha256 \""+resSum+"\"\n  end\n\n"+
		"  patch do\n    url \""+patchURL+"\"\n    sha256 \""+patchSum+"\"\n  end\n\n"+
		"  patch :DATA\n\n"+
		"  def install\n    prefix.install \"a.txt\", \"b.txt\"\n    (libexec/\"extra\").install resource(\"extra\")\n  end\nend\n"+
		"__END__\n--- a/b.txt\n+++ b/b.txt\n@@ -1 +1 @@\n-two\n+@@HOMEBREW_PREFIX@@\n")
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	keg := filepath.Join(prefix, "Cellar/tool/1.0")
	for file, want := range map[string]string{"a.txt": "uno\n", "b.txt": prefix + "\n", "libexec/extra/lib.txt": "lib\n"} {
		if b, err := os.ReadFile(filepath.Join(keg, file)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", file, b, err, want)
		}
	}
	if exists(t, prefix, "Cellar/.brw-staging") {
		t.Error("the build directory was left behind")
	}
}

// A real compile: make and clang build a library and a program that links it
// by a relative name, which the installed keg must still find.
func TestABuildCompilesAndFixesLinkage(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("brw builds from source on macOS only")
	}
	if err := exec.Command("/usr/bin/xcrun", "--find", "clang").Run(); err != nil {
		t.Skip("no compiler:", err)
	}
	e, prefix, origin, reg := tapBottleStore(t)
	url, sum := reg.AddSource("hello-1.0.tar.gz", installtest.Tarball(t, map[string]string{
		"hello-1.0/hello.c": "#include <stdio.h>\nvoid hello(void) { puts(\"hello from a dylib\"); }\n",
		"hello-1.0/main.c":  "void hello(void);\nint main(void) { hello(); return 0; }\n",
		"hello-1.0/Makefile": "all:\n\t$(CC) $(CFLAGS) $(LDFLAGS) -dynamiclib -o libhello.dylib hello.c\n" +
			"\t$(CC) $(CFLAGS) $(LDFLAGS) -o hello main.c -L. -lhello\n",
	}))
	writeTapFormula(t, origin, "hello", "class Hello < Formula\n  url \""+url+"\"\n  sha256 \""+sum+"\"\n\n"+
		"  def install\n    system \"make\"\n    bin.install \"hello\"\n    lib.install \"libhello.dylib\"\n  end\nend\n")
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/hello"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	keg := filepath.Join(prefix, "Cellar/hello/1.0")
	b, _ := os.ReadFile(filepath.Join(keg, "lib/libhello.dylib"))
	lib, err := machoedit.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := lib.DylibID(); id != filepath.Join(prefix, "opt/hello/lib/libhello.dylib") {
		t.Errorf("id = %q", id)
	}
	out, err := exec.Command(filepath.Join(prefix, "bin/hello")).CombinedOutput()
	if err != nil || string(out) != "hello from a dylib\n" {
		t.Errorf("running the installed program: %v\n%s", err, out)
	}
}

func TestABuildWritesServiceFiles(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	writeTapFormula(t, origin, "tool", strings.Replace(goreleaserTool(t, reg, "1.0"), "  def install",
		"  service do\n    run [opt_bin/\"tool\", \"serve\"]\n    log_path var/\"log/tool.log\"\n  end\n\n  def install", 1))
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	keg := filepath.Join(prefix, "Cellar/tool/1.0")
	for _, name := range []string{"sh.brew.tool.plist", "sh.brew.tool.service"} {
		fi, err := os.Stat(filepath.Join(keg, name))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o644 {
			t.Errorf("%s mode = %v", name, fi.Mode().Perm())
		}
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "sh.brew.tool.plist")); !strings.Contains(string(b), "<string>"+prefix+"/opt/tool/bin/tool</string>") {
		t.Errorf("plist:\n%s", b)
	}
	if fi, err := os.Stat(filepath.Join(prefix, "var/log")); err != nil || !fi.IsDir() {
		t.Errorf("var/log: %v", err)
	}
}

// As brew does, a build runs in HOMEBREW_TEMP, where it cannot see the
// prefix's git repository, which /opt/homebrew is.
func TestABuildRunsOutsideThePrefix(t *testing.T) {
	e, prefix, origin, reg := tapBottleStore(t)
	temp := os.Getenv("HOMEBREW_TEMP")
	gitIn(t, prefix, "init", "-q")
	writeTapFormula(t, origin, "tool", strings.Replace(goreleaserTool(t, reg, "1.0"), `bin.install "tool"`,
		`bin.install "tool"`+"\n    system \"sh\", \"-c\", \"pwd > #{prefix}/where; git rev-parse --git-dir > #{prefix}/git 2>&1 || echo none > #{prefix}/git\"", 1))
	if out, err := runCmd(t, e, "install", "-prefix", prefix, "u/t/tool"); err != nil {
		t.Fatalf("%v\n%s\n%s", err, out, e.err)
	}
	keg := filepath.Join(prefix, "Cellar/tool/1.0")
	where, _ := os.ReadFile(filepath.Join(keg, "where"))
	realTemp, _ := filepath.EvalSymlinks(temp)
	if !strings.HasPrefix(string(where), realTemp+"/tool-1.0-") {
		t.Errorf("built in %q, want under %s", where, realTemp)
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "git")); string(b) != "none\n" {
		t.Errorf("the build saw a git repository: %q", b)
	}
	if entries, _ := os.ReadDir(temp); len(entries) != 0 {
		t.Errorf("left in HOMEBREW_TEMP: %v", entries)
	}
	if !exists(t, prefix, "Cellar/tool/1.0/README.md") {
		t.Error("the metafiles were not moved into the keg")
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
