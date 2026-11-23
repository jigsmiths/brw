package rb

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// dirHost runs an install body against real files under a temp dir.
type dirHost struct {
	t   *testing.T
	ran [][]string
}

func (h *dirHost) Lstat(p string) (fs.FileInfo, error)               { return os.Lstat(p) }
func (h *dirHost) ReadDir(p string) ([]fs.DirEntry, error)           { return os.ReadDir(p) }
func (h *dirHost) ReadFile(p string) ([]byte, error)                 { return os.ReadFile(p) }
func (h *dirHost) MkdirAll(p string) error                           { return os.MkdirAll(p, 0o755) }
func (h *dirHost) Rename(o, n string) error                          { return os.Rename(o, n) }
func (h *dirHost) Symlink(target, link string) error                 { return os.Symlink(target, link) }
func (h *dirHost) Remove(p string) error                             { return os.Remove(p) }
func (h *dirHost) RemoveAll(p string) error                          { return os.RemoveAll(p) }
func (h *dirHost) Chmod(p string, m fs.FileMode) error               { return os.Chmod(p, m) }
func (h *dirHost) Glob(pattern string) ([]string, error)             { return filepath.Glob(pattern) }
func (h *dirHost) WriteFile(p string, b []byte, m fs.FileMode) error { return os.WriteFile(p, b, m) }

func (h *dirHost) Run(dir string, env map[string]string, argv []string) ([]byte, error) {
	h.ran = append(h.ran, argv)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = os.Environ()
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	return cmd.Output()
}

// installInto runs src's install body with a buildpath holding files, and
// returns the prefix.
func installInto(t *testing.T, src string, files map[string]string) (string, string, *dirHost, error) {
	t.Helper()
	asBrewHost(t, "brewuser")
	f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	prefix, build := t.TempDir(), t.TempDir()
	for name, body := range files {
		p := filepath.Join(build, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasPrefix(body, "#!") {
			mode = 0o755
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	h := &dirHost{t: t}
	err = f.Install(&InstallEnv{Host: h, Prefix: prefix, Buildpath: build})
	return prefix, filepath.Join(prefix, "Cellar", "tool", "1.0"), h, err
}

func tree(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		if d.Type()&fs.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			rel += " -> " + target
		}
		out = append(out, rel)
		return nil
	})
	sort.Strings(out)
	return out
}

const toolHead = "class Tool < Formula\n  url \"https://example.com/tool-1.0.tar.gz\"\n  def install\n"

func TestInstallMovesFilesIntoTheKeg(t *testing.T) {
	src := toolHead + `    bin.install "tool"
    bin.install "extra" => "tool2"
    bin.install_symlink "tool" => "tl"
    man1.install Dir["man/*.1"]
    (pkgshare/"note").write "hi"
    prefix.install "README.md"
  end
end
`
	_, keg, _, err := installInto(t, src, map[string]string{
		"tool": "#!/bin/sh\n", "extra": "#!/bin/sh\n", "man/tool.1": "m", "man/tool2.1": "m", "README.md": "r",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"README.md", "bin", "bin/tl -> tool", "bin/tool", "bin/tool2", "share", "share/man", "share/man/man1",
		"share/man/man1/tool.1", "share/man/man1/tool2.1", "share/tool", "share/tool/note"}
	if got := tree(t, keg); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("keg:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "share/tool/note")); string(b) != "hi" {
		t.Errorf("note = %q", b)
	}
}

// The completions come from running the installed program, as brew runs it.
func TestGenerateCompletionsRunsTheProgram(t *testing.T) {
	src := toolHead + `    bin.install "tool"
    generate_completions_from_executable(bin/"tool", "completion")
    generate_completions_from_executable(bin/"tool", shell_parameter_format: :cobra, shells: [:zsh])
  end
end
`
	_, keg, h, err := installInto(t, src, map[string]string{"tool": "#!/bin/sh\necho \"$@ $SHELL\"\n"})
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		"etc/bash_completion.d/tool":                "completion bash bash\n",
		"share/zsh/site-functions/_tool":            "completion zsh zsh\n",
		"share/fish/vendor_completions.d/tool.fish": "completion fish fish\n",
	} {
		if b, err := os.ReadFile(filepath.Join(keg, file)); err != nil || string(b) != want {
			t.Errorf("%s = %q, %v; want %q", file, b, err, want)
		}
	}
	if len(h.ran) != 4 {
		t.Errorf("ran %d commands: %v", len(h.ran), h.ran)
	}
}

func TestInstallScriptsAndEnv(t *testing.T) {
	src := toolHead + `    libexec.install "tool"
    bin.write_exec_script libexec/"tool"
    (bin/"tool-env").write_env_script libexec/"tool", TOOL_HOME: libexec
    ENV["TOOL_X"] = "1"
    system "sh", "-c", "test \"$TOOL_X\" = 1"
    cd "sub" do
      bin.install "inner"
    end
  end
end
`
	_, keg, _, err := installInto(t, src, map[string]string{"tool": "#!/bin/sh\n", "sub/inner": "#!/bin/sh\n"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(keg, "bin/tool"))
	if want := "#!/bin/bash\nexec \"" + filepath.Join(keg, "libexec/tool") + "\" \"$@\"\n"; string(b) != want {
		t.Errorf("exec script = %q, want %q", b, want)
	}
	b, _ = os.ReadFile(filepath.Join(keg, "bin/tool-env"))
	if want := "#!/bin/bash\nTOOL_HOME=\"" + filepath.Join(keg, "libexec") + "\" exec \"" + filepath.Join(keg, "libexec/tool") + "\"  \"$@\"\n"; string(b) != want {
		t.Errorf("env script = %q, want %q", b, want)
	}
	if fi, err := os.Stat(filepath.Join(keg, "bin/tool-env")); err != nil || fi.Mode().Perm() != 0o555 {
		t.Errorf("env script mode: %v %v", fi, err)
	}
	if _, err := os.Stat(filepath.Join(keg, "bin/inner")); err != nil {
		t.Error(err)
	}
}

// Ruby reads 0555 as octal, as a formula's chmod means it.
func TestChmodTakesOctalLiterals(t *testing.T) {
	src := toolHead + "    bin.install \"tool\"\n    chmod 0555, bin/\"tool\"\n    (bin/\"x\").write \"\"\n    chmod 0o700, bin/\"x\"\n  end\nend\n"
	_, keg, _, err := installInto(t, src, map[string]string{"tool": "plain"})
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]os.FileMode{"bin/tool": 0o555, "bin/x": 0o700} {
		if fi, err := os.Stat(filepath.Join(keg, file)); err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s: %v %v, want %o", file, fi.Mode(), err, want)
		}
	}
}

// The std_*_args are brew's, and reach the build's commands.
func TestStdArgsAreBrews(t *testing.T) {
	src := toolHead + `    system "sh", "-c", "echo \"$*\" > configure.out", "-", *std_configure_args
    system "sh", "-c", "echo \"$*\" > cmake.out", "-", *std_cmake_args
    system "sh", "-c", "echo \"$*\" > go.out", "-", *std_go_args(ldflags: "-X main.v=1")
    system "sh", "-c", "echo \"$*\" > cargo.out", "-", *std_cargo_args
    system "sh", "-c", "echo \"$*\" > meson.out", "-", *std_meson_args
    prefix.install Dir["*.out"]
  end
end
`
	prefix, keg, _, err := installInto(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	for file, want := range map[string]string{
		"configure.out": "--disable-debug --disable-dependency-tracking --prefix=" + keg + " --libdir=" + keg + "/lib",
		"cmake.out": "-DCMAKE_INSTALL_PREFIX=" + keg + " -DCMAKE_INSTALL_LIBDIR=lib -DCMAKE_BUILD_TYPE=Release " +
			"-DCMAKE_FIND_FRAMEWORK=LAST -DCMAKE_VERBOSE_MAKEFILE=ON -Wno-author -DBUILD_TESTING=OFF -DCCACHE_FOUND=OFF",
		"go.out":    "-trimpath -o=" + keg + "/bin/tool -ldflags=-s -w -X main.v=1",
		"cargo.out": "--jobs " + strconv.Itoa(runtime.NumCPU()) + " --locked --root=" + keg + " --path=.",
		"meson.out": "--prefix=" + keg + " --libdir=lib --buildtype=release --wrap-mode=nofallback",
	} {
		b, err := os.ReadFile(filepath.Join(keg, file))
		if err != nil || strings.TrimSpace(string(b)) != want {
			t.Errorf("%s = %q, %v\nwant %q", file, b, err, want)
		}
	}
	_ = prefix
}

// std_cmake_args traps FetchContent with brew's file when the prefix has it.
func TestStdCmakeArgsUseBrewsTrapWhenThere(t *testing.T) {
	asBrewHost(t, "brewuser")
	f, err := LoadFormula([]byte(toolHead+"    (prefix/\"a\").write std_cmake_args.join(\" \")\n  end\nend\n"), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	prefix, build := t.TempDir(), t.TempDir()
	trap := filepath.Join(prefix, "Library/Homebrew/cmake/trap_fetchcontent_provider.cmake")
	if err := os.MkdirAll(filepath.Dir(trap), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trap, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.Install(&InstallEnv{Host: &dirHost{t: t}, Prefix: prefix, Buildpath: build}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "Cellar/tool/1.0/a")); !strings.Contains(string(b), "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES="+trap) {
		t.Errorf("args = %s", b)
	}
}

func TestEnvMethodsAndInreplaceBlocks(t *testing.T) {
	src := toolHead + `    ENV["CFLAGS"] = "-O2"
    ENV.append "CFLAGS", "-g"
    ENV.prepend_path "PATH", "/opt/x/bin"
    ENV.deparallelize
    inreplace "Makefile" do |s|
      s.gsub! "@PREFIX@", prefix
      s.sub!(/^CC=.*$/, "CC=clang")
      s.change_make_var! "OPT", "-Os"
    end
    system "sh", "-c", "echo \"$CFLAGS|$MAKEFLAGS|${PATH%%:*}\" > env.out"
    prefix.install "Makefile", "env.out"
  end
end
`
	h := map[string]string{"Makefile": "CC=gcc\nOPT=-O0 -g\nPREFIX=@PREFIX@\nDEST=@PREFIX@/bin\n"}
	asBrewHost(t, "brewuser")
	f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	prefix, build := t.TempDir(), t.TempDir()
	for name, body := range h {
		if err := os.WriteFile(filepath.Join(build, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	env := &InstallEnv{Host: &dirHost{t: t}, Prefix: prefix, Buildpath: build, Env: map[string]string{"MAKEFLAGS": "-j8", "PATH": "/usr/bin"}}
	if err := f.Install(env); err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(prefix, "Cellar/tool/1.0")
	if b, _ := os.ReadFile(filepath.Join(keg, "Makefile")); string(b) != "CC=clang\nOPT=-Os\nPREFIX="+keg+"\nDEST="+keg+"/bin\n" {
		t.Errorf("Makefile = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "env.out")); string(b) != "-O2 -g||/opt/x/bin\n" {
		t.Errorf("env = %q", b)
	}
}

func TestPatchesAndResourcesOfAFormula(t *testing.T) {
	asBrewHost(t, "brewuser")
	src := `class Tool < Formula
  url "https://example.com/tool-1.0.tar.gz"
  sha256 "aa"

  resource "extra" do
    url "https://example.com/extra-2.0.tar.gz"
    sha256 "bb"
  end

  patch do
    url "https://example.com/fix.patch"
    sha256 "cc"
  end

  patch :p0, :DATA

  def install
    (libexec/"extra").install resource("extra")
    resource("extra").stage do
      system "sh", "-c", "pwd > #{prefix}/staged-in"
    end
    resources.each { |r| (prefix/"names").write r.name }
  end
end
__END__
--- a
+++ b
`
	f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Resources(); len(got) != 1 || got[0] != (Resource{Name: "extra", URL: "https://example.com/extra-2.0.tar.gz", SHA256: "bb"}) {
		t.Errorf("resources = %+v", got)
	}
	want := []Patch{{Strip: "p1", URL: "https://example.com/fix.patch", SHA256: "cc"}, {Strip: "p0", Data: "--- a\n+++ b\n"}}
	if got := f.Patches(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("patches = %+v", got)
	}

	prefix, build, scratch := t.TempDir(), t.TempDir(), t.TempDir()
	h := &dirHost{t: t}
	env := &InstallEnv{Host: h, Prefix: prefix, Buildpath: build, Scratch: scratch,
		Resources: map[string]string{"extra": "the archive"},
		Unpack: func(archive, dir string) error {
			if archive != "the archive" {
				t.Errorf("unpacked %s", archive)
			}
			return os.WriteFile(filepath.Join(dir, "lib.txt"), []byte("lib"), 0o644)
		}}
	if err := f.Install(env); err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(prefix, "Cellar/tool/1.0")
	if b, err := os.ReadFile(filepath.Join(keg, "libexec/extra/lib.txt")); err != nil || string(b) != "lib" {
		t.Errorf("installed resource: %q, %v", b, err)
	}
	real, _ := filepath.EvalSymlinks(scratch) // pwd prints /private/var for /var
	if b, _ := os.ReadFile(filepath.Join(keg, "staged-in")); !strings.HasPrefix(string(b), real) {
		t.Errorf("staged in %q, want under %s", b, scratch)
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "names")); string(b) != "extra" {
		t.Errorf("names = %q", b)
	}
}

func TestPostInstallRunsTheFormulasMethod(t *testing.T) {
	asBrewHost(t, "brewuser")
	src := `class Tool < Formula
  url "https://example.com/tool-1.0.tar.gz"
  sha256 "aa"

  def install
    bin.mkpath
  end

  def post_install
    (var/"tool").mkpath
    (var/"log/tool").mkpath
    if !(File.exist?((etc/"tool.conf"))) then
      (etc/"tool.conf").write tool_conf
    end
    ohai "To finish, open Tool.app."
    puts "It is in: #{prefix}"
  end

  def tool_conf
    <<~EOS
      dbpath = #{var}/tool
    EOS
  end
end
`
	f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	prefix, home := t.TempDir(), t.TempDir()
	var out strings.Builder
	env := &InstallEnv{Host: &dirHost{t: t}, Prefix: prefix, Buildpath: home, Out: &out}
	if err := f.PostInstall(env); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(prefix, "etc/tool.conf")); err != nil || string(b) != "dbpath = "+prefix+"/var/tool\n" {
		t.Errorf("tool.conf = %q, %v", b, err)
	}
	for _, d := range []string{"var/tool", "var/log/tool"} {
		if fi, err := os.Stat(filepath.Join(prefix, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s: %v", d, err)
		}
	}
	if want := "==> To finish, open Tool.app.\nIt is in: " + prefix + "/Cellar/tool/1.0\n"; out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}

	// A second run keeps the user's configuration.
	if err := os.WriteFile(filepath.Join(prefix, "etc/tool.conf"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := f.PostInstall(env); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "etc/tool.conf")); string(b) != "mine" {
		t.Errorf("tool.conf was overwritten: %q", b)
	}
}

func TestExpandPathUsesTheInstallHome(t *testing.T) {
	asBrewHost(t, "brewuser")
	f, err := LoadFormula([]byte(toolHead+"    (prefix/\"p\").write [File.expand_path(\"~/Library/a.plist\"), File.expand_path(\"~\"), File.expand_path(\"x\")].join(\",\")\n  end\nend\n"), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	prefix, build := t.TempDir(), t.TempDir()
	if err := f.Install(&InstallEnv{Host: &dirHost{t: t}, Prefix: prefix, Buildpath: build, Env: map[string]string{"HOME": "/h"}}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(prefix, "Cellar/tool/1.0/p")); string(b) != "/h/Library/a.plist,/h,"+build+"/x" {
		t.Errorf("got %s", b)
	}
}

// brw takes no install options, so build answers as brew's BuildOptions does
// when none were given: only a without- option counts as with.
func TestBuildOptionsWithoutAnyGiven(t *testing.T) {
	src := toolHead + `    got = []
    got << build.with?("test-commands")
    got << build.with?("extra")
    got << build.without?("extra")
    got << build.with?("undefined")
    got << build.head?
    got << build.stable?
    got << build.bottle?
    (prefix/"got").write got.join(",")
  end

  option "with-test-commands", "Enable test commands"
  depends_on "extra" => :recommended
end
`
	_, keg, _, err := installInto(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "got")); string(b) != "false,true,false,false,false,true,false" {
		t.Errorf("got %s", b)
	}
}

func TestInstallFailures(t *testing.T) {
	for name, body := range map[string]string{
		"a missing file":   `    bin.install "nope"`,
		"a failed command": `    system "false"`,
	} {
		_, _, _, err := installInto(t, toolHead+body+"\n  end\nend\n", nil)
		if err == nil || errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want a build failure", name, err)
		}
	}
	_, _, _, err := installInto(t, toolHead+"    virtualenv_install_with_resources\n  end\nend\n", nil)
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("unknown method: %v, want ErrUnsupported", err)
	}
}
