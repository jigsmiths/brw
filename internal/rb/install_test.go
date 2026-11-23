package rb

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
