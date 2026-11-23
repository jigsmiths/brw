package rb

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

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

// The file:///dev/null meta packages rely on Pathname#+ joining.
func TestPathnamePlusJoins(t *testing.T) {
	src := toolHead + `    (bin+"meta").write "m"
    (prefix + "/elsewhere").to_s
    (share+"a"+"b").write "ab"
    (Pathname.new(prefix) + "c").write "c"
    (lib/"d").write "bin" + "dir"
  end
end
`
	_, keg, _, err := installInto(t, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bin", "bin/meta", "c", "lib", "lib/d", "share", "share/a", "share/a/b"}
	if got := tree(t, keg); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("keg:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "lib/d")); string(b) != "bindir" {
		t.Errorf("lib/d = %q", b)
	}
	if got := pathPlus("/p/bin", "/elsewhere"); got != "/elsewhere" {
		t.Errorf("pathPlus with an absolute path = %q", got)
	}
}

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

func TestGoreleaserLdflags(t *testing.T) {
	asBrewHost(t, "brewuser")
	for _, tt := range []struct{ url, commit, want string }{
		{`url "https://example.com/tool-1.0.tar.gz"`, "abc123", "abc123"},
		{`url "https://example.com/tool-1.0.tar.gz"`, "", "acme"},
		{`url "https://example.com/tool.git", tag: "v1.0", revision: "fff000"`, "abc123", "fff000"},
	} {
		src := "class Tool < Formula\n  " + tt.url + "\n  version \"1.0\"\n  def install\n" +
			"    (prefix/\"args\").write std_go_args(ldflags: :goreleaser).join(\"\\n\")\n  end\nend\n"
		f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
		if err != nil {
			t.Fatal(err)
		}
		prefix := t.TempDir()
		env := &InstallEnv{Host: &dirHost{t: t}, Prefix: prefix, Buildpath: t.TempDir(), Tap: "acme/tools", TarCommit: tt.commit,
			Env: map[string]string{"SOURCE_DATE_EPOCH": "1795425211"}}
		if err := f.Install(env); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(filepath.Join(prefix, "Cellar/tool/1.0/args"))
		want := "-trimpath\n-o=" + prefix + "/Cellar/tool/1.0/bin/tool\n-ldflags=-s -w -X 'main.version=1.0' -X 'main.commit=" + tt.want +
			"' -X 'main.date=2026-11-23T09:13:31Z' -X 'main.builtBy=acme'"
		if string(b) != want {
			t.Errorf("%s:\n%s\nwant\n%s", tt.url, b, want)
		}
	}
}

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

func TestShellParameterIsBrews(t *testing.T) {
	for _, tt := range []struct {
		format     value
		shell      string
		args       []string
		envKey, ev string
	}{
		{nil, "zsh", []string{"zsh"}, "", ""},
		{sym("arg"), "pwsh", []string{"--shell=powershell"}, "", ""},
		{sym("clap"), "fish", nil, "COMPLETE", "fish"},
		{sym("click"), "bash", nil, "_MY_TOOL_COMPLETE", "bash_source"},
		{sym("cobra"), "pwsh", []string{"completion", "powershell"}, "", ""},
		{sym("flag"), "bash", []string{"--bash"}, "", ""},
		{sym("none"), "bash", nil, "", ""},
		{sym("typer"), "zsh", []string{"--show-completion", "zsh"}, "_TYPER_COMPLETE_TEST_DISABLE_SHELL_DETECTION", "1"},
		{rstr("--completions="), "zsh", []string{"--completions=zsh"}, "", ""},
	} {
		env := map[string]string{}
		got := shellParameter(tt.format, tt.shell, "/opt/x/bin/my-tool", env)
		if !reflect.DeepEqual(got, tt.args) || (tt.envKey != "" && env[tt.envKey] != tt.ev) {
			t.Errorf("%v %s: %q %v, want %q %s=%s", tt.format, tt.shell, got, env, tt.args, tt.envKey, tt.ev)
		}
	}
}

func TestEnvMethodsAreBrews(t *testing.T) {
	src := toolHead + `    ENV["A"] = "x"
    ENV.append ["A", "B"], "y"
    ENV.prepend "A", "w", ":"
    ENV.append_path "P", "/a"
    ENV.append_path "P", "/usr/bin"
    ENV.prepend_path "P", "/bin"
    ENV.prepend_path "P", "/a"
    ENV.prepend_create_path "Q", buildpath/"q/lib"
    ENV["CFLAGS"] = "-Os -w"
    ENV.O0
    ENV.deparallelize do
      system "sh", "-c", "echo \"[$MAKEFLAGS]\" > inside"
    end
    ENV.delete "B"
    ENV["JOBS"] = ENV.make_jobs.to_s
    system "sh", "-c", "echo \"$A|${B-unset}|$P|$Q|$CFLAGS|$CXXFLAGS|$MAKEFLAGS|$JOBS\" > env.out"
    prefix.install "env.out", "inside"
  end
end
`
	asBrewHost(t, "brewuser")
	lookupEnv = func(k string) (string, bool) { return "3", k == "HOMEBREW_MAKE_JOBS" }
	f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	prefix, build := t.TempDir(), t.TempDir()
	env := &InstallEnv{Host: &dirHost{t: t}, Prefix: prefix, Buildpath: build, Env: map[string]string{"MAKEFLAGS": "-j8", "P": "/usr/bin"}}
	if err := f.Install(env); err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(prefix, "Cellar/tool/1.0")
	want := "w:x y|unset|/a:/usr/bin|" + build + "/q/lib| -w -O0|-O0|-j8|3\n"
	if b, _ := os.ReadFile(filepath.Join(keg, "env.out")); string(b) != want {
		t.Errorf("env = %q\nwant  %q", b, want)
	}
	if b, _ := os.ReadFile(filepath.Join(keg, "inside")); string(b) != "[]\n" {
		t.Errorf("MAKEFLAGS inside deparallelize = %q", b)
	}
	if fi, err := os.Stat(filepath.Join(build, "q/lib")); err != nil || !fi.IsDir() {
		t.Errorf("prepend_create_path made no directory: %v", err)
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

func TestFileMethodsOnRealFiles(t *testing.T) {
	files := map[string]string{"a.txt": "a\n", "dir/b.txt": "b\n", "dir/sub/c.txt": "c\n", "tool": "#!/bin/sh\necho t\n"}
	for _, tt := range []struct{ name, body, out string }{
		{"mkdir, touch, cp and mv", `mkdir_p "x/y"
    touch "x/y/new"
    touch "a.txt"
    cp "a.txt", "x"
    cp_r "dir", "copy"
    mv "copy/b.txt", "moved.txt"
    FileUtils.mkdir "z"
    (prefix/"out").write [File.read("x/a.txt"), File.exist?("x/y/new"), File.read("a.txt"),
      Dir.exist?("copy/sub"), File.file?("moved.txt"), File.exist?("copy/b.txt"), Dir.exist?("z")].join(",")`,
			"a\n,true,a\n,true,true,false,true"},
		{"links and removal", `ln_s "a.txt", "link"
    ln_sf "dir", "link"
    rm "a.txt"
    rm_f "missing"
    rm_rf "dir/sub"
    rmdir "dir" rescue nil
    (prefix/"out").write [File.symlink?("link"), File.exist?("a.txt"), Dir.exist?("dir/sub")].join(",")`,
			"true,false,false"},
		{"chmod", `chmod 0700, "tool"
    File.chmod 0600, "a.txt"
    (buildpath/"dir/b.txt").chmod 0640
    prefix.install "tool", "a.txt", "dir/b.txt"
    (prefix/"out").write "ok"`, "ok"},
		{"rescue", `begin
      system "false"
      (prefix/"never").write "x"
    rescue
      (prefix/"rescued").write "y"
    ensure
      (prefix/"ensured").write "z"
    end
    got = (system("false") rescue "fallback")
    (prefix/"out").write [got, File.exist?(prefix/"never"), File.read(prefix/"rescued"), File.read(prefix/"ensured")].join(",")`,
			"fallback,false,y,z"},
		{"pathnames", `p = buildpath/"dir"
    (p/"new.txt").write "n"
    (p/"new.txt").append_lines "m"
    (p/"gone").atomic_write "g"
    (p/"gone").unlink
    (buildpath/"made/deep").mkpath
    (buildpath/"made").rmtree
    names = p.children.map { |c| File.basename(c) }.sort
    (prefix/"out").write [(p/"new.txt").read, names.join(" "), p.directory?, (p/"b.txt").file?,
      (p/"b.txt").basename, (p/"b.txt").extname, (p/"sub/c.txt").dirname.basename,
      (p/"sub/c.txt").relative_path_from(p), p.glob("*.txt").map { |c| File.basename(c) }.sort.join(" "),
      Dir.exist?("made")].join(",")`,
			"nm\n,b.txt new.txt sub,true,true,b.txt,.txt,sub,sub/c.txt,b.txt new.txt,false"},
		{"file and dir class methods", `(prefix/"out").write [File.join("a", "b", "c"), File.basename("x/y.tar.gz", ".gz"),
      File.dirname("x/y/z"), Dir.glob("dir/**/*.txt").sort.join(" "), Dir.pwd == buildpath.to_s,
      File.expand_path("q") == (buildpath/"q").to_s].join(",")`,
			"a/b/c,y.tar,x/y,dir/b.txt dir/sub/c.txt,true,true"},
		{"scripts", `(libexec/"bin").install "tool"
    bin.write_exec_script libexec/"bin/tool"
    (bin/"env").write_env_script libexec/"bin/tool", FOO: "1"
    (bin/"rel").make_relative_symlink libexec/"bin/tool"
    (prefix/"out").write [File.read(bin/"tool"), File.read(bin/"env")].join("|")`,
			"#!/bin/bash\nexec \"$keg/libexec/bin/tool\" \"$@\"\n|#!/bin/bash\nFOO=\"1\" exec \"$keg/libexec/bin/tool\"  \"$@\"\n"},
		{"popen and cd", `out = Utils.safe_popen_read("sh", "-c", "echo $X", ) rescue "x"
    got = Utils.safe_popen_read({ "X" => "1" }, "sh", "-c", "echo $X").chomp
    cd "dir" do
      system "sh", "-c", "pwd > ../where"
    end
    (prefix/"out").write [got, File.basename(File.read("where").chomp)].join(",")`,
			"1,dir"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, keg, _, err := installInto(t, toolHead+"    "+tt.body+"\n  end\nend\n", files)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(keg, "out"))
			if want := strings.ReplaceAll(tt.out, "$keg", keg); err != nil || string(b) != want {
				t.Errorf("out = %q, %v\nwant  %q", b, err, want)
			}
			switch tt.name {
			case "chmod":
				for name, want := range map[string]os.FileMode{"tool": 0o700, "a.txt": 0o600, "b.txt": 0o640} {
					if fi, err := os.Stat(filepath.Join(keg, name)); err != nil || fi.Mode().Perm() != want {
						t.Errorf("%s: %v, want %v", name, fi.Mode().Perm(), want)
					}
				}
			case "scripts":
				if l, _ := os.Readlink(filepath.Join(keg, "bin/rel")); l != "../libexec/bin/tool" {
					t.Errorf("bin/rel -> %q", l)
				}
				if fi, _ := os.Stat(filepath.Join(keg, "bin/env")); fi.Mode().Perm() != 0o555 {
					t.Errorf("bin/env mode = %v", fi.Mode().Perm())
				}
			}
		})
	}
}

func TestInreplaceIsBrews(t *testing.T) {
	makefile := "CC=gcc\nOPT += -O0 \\\n  -g\nDEBUG:=1\nKEEP=1\nPREFIX=/usr/local\nNAME=a a\n"
	for _, tt := range []struct {
		name, body, want, err string
	}{
		{"make vars", `inreplace "Makefile" do |s|
      s.change_make_var! "OPT", "-Os"
      s.remove_make_var! ["DEBUG", "KEEP"]
      s.gsub! "/usr/local", prefix
      s.sub! /=a/, "=b"
      s.gsub! "NAME", "NAME"
      (prefix/"cc").write s.get_make_var("CC")
    end`, "CC=gcc\nOPT=-Os\nPREFIX=$keg\nNAME=b a\n", ""},
		{"one edit fails", `inreplace "Makefile" do |s|
      s.gsub! "CC=gcc", "CC=clang"
      s.gsub! "nowhere", "x"
    end`, "", `expected replacement of "nowhere" with "x"`},
		{"not audited", `inreplace "Makefile" do |s|
      s.gsub! "nowhere", "x", audit_result: false
      s.gsub! "CC=gcc", "CC=clang"
    end`, "CC=clang\nOPT += -O0 \\\n  -g\nDEBUG:=1\nKEEP=1\nPREFIX=/usr/local\nNAME=a a\n", ""},
		{"unknown make var", `inreplace "Makefile" do |s|
      s.change_make_var! "NOPE", "1"
    end`, "", `expected to change "NOPE" to "1"`},
		{"same text", `inreplace "Makefile", "KEEP=1", "KEEP=1"`, makefile, ""},
		{"first only", `inreplace "Makefile", "a", "b", global: false`, "CC=gcc\nOPT += -O0 \\\n  -g\nDEBUG:=1\nKEEP=1\nPREFIX=/usr/locbl\nNAME=a a\n", ""},
		{"no match", `inreplace "Makefile", "nowhere", "x"`, "", `expected replacement of "nowhere" with "x"`},
		{"no match allowed", `inreplace "Makefile", "nowhere", "x", audit_result: false`, makefile, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, keg, _, err := installInto(t, toolHead+"    "+tt.body+"\n    prefix.install \"Makefile\"\n  end\nend\n",
				map[string]string{"Makefile": makefile})
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(filepath.Join(keg, "Makefile"))
			if want := strings.ReplaceAll(tt.want, "$keg", keg); string(b) != want {
				t.Errorf("Makefile = %q\nwant       %q", b, want)
			}
			if tt.name == "make vars" {
				if b, _ := os.ReadFile(filepath.Join(keg, "cc")); string(b) != "gcc" {
					t.Errorf("get_make_var = %q", b)
				}
			}
		})
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

// A HEAD build installs the head spec: its version, resources and patches,
// with the class-level ones it shares with stable.
func TestInstallingTheHeadSpec(t *testing.T) {
	asBrewHost(t, "brewuser")
	src := `class Tool < Formula
  url "https://example.com/tool-1.0.tar.gz", revision: "stablecommit"
  sha256 "aa"
  revision 1

  head do
    url "https://example.com/tool.git", branch: "main"
    resource "headonly" do
      url "https://example.com/headonly-1.0.tar.gz"
      sha256 "bb"
    end
    patch :DATA
  end

  resource "shared" do
    url "https://example.com/shared-1.0.tar.gz"
    sha256 "cc"
  end

  def install
    got = [version, pkg_version, build.head?, build.stable?, resources.map(&:name).join("+")]
    (prefix/"got").write got.join(",")
    system "echo", *std_go_args(ldflags: :goreleaser)
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
	if got, want := f.Head(), (&HeadSource{URL: "https://example.com/tool.git", Branch: "main"}); got == nil || *got != *want {
		t.Errorf("head = %+v, want %+v", got, want)
	}
	f.UseHead("HEAD-abc1234")
	if got := f.Resources(); len(got) != 2 || got[0].Name != "headonly" || got[1].Name != "shared" {
		t.Errorf("resources = %+v", got)
	}
	if got := f.Patches(); len(got) != 1 || got[0].Data != "--- a\n+++ b\n" {
		t.Errorf("patches = %+v", got)
	}
	prefix, build := t.TempDir(), t.TempDir()
	h := &dirHost{t: t}
	env := &InstallEnv{Host: h, Prefix: prefix, Buildpath: build, TarCommit: "abc1234def"}
	if err := f.Install(env); err != nil {
		t.Fatal(err)
	}
	keg := filepath.Join(prefix, "Cellar/tool/HEAD-abc1234_1")
	if b, err := os.ReadFile(filepath.Join(keg, "got")); err != nil || string(b) != "HEAD-abc1234,HEAD-abc1234_1,true,false,headonly+shared" {
		t.Errorf("got %q, %v", b, err)
	}
	if args := strings.Join(h.ran[len(h.ran)-1], " "); !strings.Contains(args, "-X 'main.version=HEAD-abc1234' -X 'main.commit=abc1234def'") {
		t.Errorf("std_go_args: %s", args)
	}

	stable, _ := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if got := stable.Resources(); len(got) != 1 || got[0].Name != "shared" {
		t.Errorf("stable resources = %+v", got)
	}
	if noHead, _ := LoadFormula([]byte(toolHead+"  end\nend\n"), "tool", "arm64_golden_gate"); noHead.Head() != nil {
		t.Errorf("a formula without a head has one: %+v", noHead.Head())
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
