package cask

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestReadGenerator(t *testing.T) {
	g, err := readGenerator(json.RawMessage(`["bin/tool","completion",{"base_name":null,"shell_parameter_format":null,"shells":["bash","zsh","fish"]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(g.commands, []string{"bin/tool", "completion"}) || g.baseName != "" || g.format != "" || !slices.Equal(g.shells, []string{"bash", "zsh", "fish"}) {
		t.Errorf("generator = %+v", g)
	}
	g, err = readGenerator(json.RawMessage(`["x",{"shell_parameter_format":"cobra"}]`))
	if err != nil || !slices.Equal(g.shells, []string{"bash", "zsh", "fish", "pwsh"}) {
		t.Errorf("cobra defaults to four shells: %+v, %v", g, err)
	}
	g, err = readGenerator(json.RawMessage(`["x"]`))
	if err != nil || !slices.Equal(g.shells, []string{"bash", "zsh", "fish"}) {
		t.Errorf("defaults to three shells: %+v, %v", g, err)
	}
	for _, bad := range []string{`[{"shells":["bash"]}]`, `["x",{"shells":["tcsh"]}]`, `"x"`} {
		if _, err := readGenerator(json.RawMessage(bad)); err == nil {
			t.Errorf("readGenerator(%s) accepted", bad)
		}
	}
}

func TestShellParameter(t *testing.T) {
	tests := []struct {
		format, shell string
		args, env     []string
	}{
		{"", "bash", []string{"bash"}, nil},
		{"", "pwsh", []string{"powershell"}, nil},
		{"arg", "zsh", []string{"--shell=zsh"}, nil},
		{"flag", "fish", []string{"--fish"}, nil},
		{"cobra", "pwsh", []string{"completion", "powershell"}, nil},
		{"clap", "bash", nil, []string{"COMPLETE=bash"}},
		{"click", "zsh", nil, []string{"_MY_TOOL_COMPLETE=zsh_source"}},
		{"typer", "fish", []string{"--show-completion", "fish"}, []string{"_TYPER_COMPLETE_TEST_DISABLE_SHELL_DETECTION=1"}},
		{"none", "bash", nil, nil},
		{"--autocomplete=init:", "pwsh", []string{"--autocomplete=init:pwsh"}, nil},
	}
	for _, tt := range tests {
		args, env := shellParameter(tt.format, tt.shell, "/opt/x/bin/my-tool")
		if !slices.Equal(args, tt.args) || !slices.Equal(env, tt.env) {
			t.Errorf("shellParameter(%q, %s) = %q, %q; want %q, %q", tt.format, tt.shell, args, env, tt.args, tt.env)
		}
	}
}

func TestCompletionPaths(t *testing.T) {
	for _, tt := range []struct{ name, shell, want string }{
		{"zed", "bash", "etc/bash_completion.d/zed"},
		{"zed", "zsh", "share/zsh/site-functions/_zed"},
		{"zed", "fish", "share/fish/vendor_completions.d/zed.fish"},
		{"zed", "pwsh", "share/pwsh/completions/_zed.ps1"},
		{"tool.sh", "bash", "etc/bash_completion.d/tool"},
		{"_tool", "zsh", "share/zsh/site-functions/_tool"},
		{"tool.fish", "fish", "share/fish/vendor_completions.d/tool.fish"},
		{"_tool.ps1", "pwsh", "share/pwsh/completions/_tool.ps1"},
	} {
		if got := completionPath(tt.shell, tt.name); got != tt.want {
			t.Errorf("completionPath(%s, %s) = %s, want %s", tt.shell, tt.name, got, tt.want)
		}
	}
	for _, tt := range []struct {
		g    generator
		want string
	}{
		{generator{commands: []string{"$APPDIR/X.app/Contents/MacOS/cli"}}, "cli"},
		{generator{commands: []string{"grok-1.0-macos"}, baseName: "grok"}, "grok"},
		{generator{commands: []string{"/"}}, "app"},
	} {
		if got := tt.g.name("app"); got != tt.want {
			t.Errorf("%+v name = %s, want %s", tt.g, got, tt.want)
		}
	}
}

func TestSandboxProfile(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(home, "Applications")
	if err := os.MkdirAll(filepath.Join(home, "Documents"), 0o700); err != nil {
		t.Fatal(err)
	}
	p := sandboxProfile(home, []string{filepath.Join(home, "Documents", "prefix"), inside})
	for _, want := range []string{"(version 1)", "(deny network*)", "(deny file-write*)", `(subpath "/private/tmp")`, `(deny file-read* (subpath "` + resolved(filepath.Join(home, ".ssh")) + `"))`} {
		if !strings.Contains(p, want) {
			t.Errorf("profile lacks %s:\n%s", want, p)
		}
	}
	if strings.Contains(p, resolved(filepath.Join(home, "Documents"))) {
		t.Errorf("profile denies a folder holding a path the generator needs:\n%s", p)
	}
	if strings.Contains(p, filepath.Join(home, ".aws")) {
		t.Errorf("profile names a folder that is not there:\n%s", p)
	}
}

// completionArtifacts is a cask with a binary whose completions it generates.
const completionArtifacts = `[
	{"binary":["tool"]},
	{"generate_completions_from_executable":["$HOMEBREW_PREFIX/bin/tool","completion",{"base_name":null,"shell_parameter_format":null,"shells":["bash","zsh"]}]}]`

// generating answers for the generator as if each shell's script were "<shell> script".
func generating(f *fakeExec, failShell string) {
	run := func(args []string) ([]byte, error) {
		for _, a := range args {
			if shell, ok := strings.CutPrefix(a, "SHELL="); ok {
				if shell == failShell {
					return nil, errors.New("exit status 1")
				}
				return []byte(shell + " script\n"), nil
			}
		}
		return nil, errors.New("no SHELL")
	}
	f.handle = map[string]func([]string) ([]byte, error){sandboxExec: run, "/usr/bin/env": run}
}

func TestInstallGeneratesCompletions(t *testing.T) {
	t.Setenv("MY_API_TOKEN", "secret")
	ti := newTestInstall(t, appTarball)
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}}
	generating(f, "")
	ti.in.Exec = f.exec
	if err := ti.in.Install(context.Background(), ti.cask(t, completionArtifacts)); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"etc/bash_completion.d/tool":     "bash script\n",
		"share/zsh/site-functions/_tool": "zsh script\n",
	} {
		if got := readFile(t, filepath.Join(ti.prefix, path)); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	tool := filepath.Join(ti.prefix, "bin/tool")
	var runs []string
	for _, c := range f.calls {
		if strings.Contains(c, "SHELL=") {
			runs = append(runs, c)
		}
	}
	if len(runs) != 2 || !strings.HasSuffix(runs[0], " SHELL=bash "+tool+" completion bash") ||
		!strings.HasSuffix(runs[1], " SHELL=zsh "+tool+" completion zsh") {
		t.Errorf("generator runs = %q", runs)
	}
	// As brew: a home of its own, and no secrets from the environment.
	if !strings.Contains(runs[0], "/usr/bin/env ") || !strings.Contains(runs[0], " -u MY_API_TOKEN ") ||
		!strings.Contains(runs[0], " HOME=") || !strings.Contains(runs[0], " -C ") {
		t.Errorf("generator run = %q", runs[0])
	}
	receipt := readFile(t, filepath.Join(ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if !strings.Contains(receipt, `"generate_completions_from_executable": [`) || !strings.Contains(receipt, `"`+tool+`"`) {
		t.Errorf("the receipt does not record the generator:\n%s", receipt)
	}

	if err := ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"etc/bash_completion.d/tool", "share/zsh/site-functions/_tool"} {
		if exists(filepath.Join(ti.prefix, p)) {
			t.Errorf("uninstall left %s", p)
		}
	}
}

func TestInstallWarnsWhenACompletionFails(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}}
	generating(f, "zsh")
	ti.in.Exec = f.exec
	if err := ti.in.Install(context.Background(), ti.cask(t, completionArtifacts)); err != nil {
		t.Fatalf("a failed completion failed the install: %v", err)
	}
	want := "Warning: Failed to generate zsh completions from " + filepath.Join(ti.prefix, "bin/tool") + ": exit status 1\n"
	if !strings.Contains(ti.warn.String(), want) {
		t.Errorf("warnings = %q, want %q", ti.warn, want)
	}
	if !exists(filepath.Join(ti.prefix, "etc/bash_completion.d/tool")) || exists(filepath.Join(ti.prefix, "share/zsh/site-functions/_tool")) {
		t.Error("want only the bash completion")
	}
}

func TestInstallRunsAGeneratorInTheStagedVersion(t *testing.T) {
	ti := newTestInstall(t, appTarball)
	f := &fakeExec{running: map[string]int{}, fail: map[string]bool{}}
	generating(f, "")
	ti.in.Exec = f.exec
	artifacts := `[{"binary":["tool"]},{"generate_completions_from_executable":["tool",{"base_name":"tl","shell_parameter_format":"cobra","shells":["fish"]}]}]`
	if err := ti.in.Install(context.Background(), ti.cask(t, artifacts)); err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(ti.prefix, "Caskroom/app/1.0/tool")
	if got := f.ran(""); !slices.ContainsFunc(got, func(c string) bool {
		return strings.HasSuffix(c, " SHELL=fish "+staged+" completion fish")
	}) {
		t.Errorf("calls = %q", got)
	}
	if got := readFile(t, filepath.Join(ti.prefix, "share/fish/vendor_completions.d/tl.fish")); got != "fish script\n" {
		t.Errorf("tl.fish = %q", got)
	}
}

func TestUpgradeRegeneratesCompletions(t *testing.T) {
	ti, f, c := upgradable(t, completionArtifacts)
	generating(f, "")
	bash := filepath.Join(ti.prefix, "etc/bash_completion.d/tool")
	writeFile(t, bash, []byte("old\n"), 0o644)
	if err := ti.in.Upgrade(context.Background(), installedCask(t, ti.prefix), c); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, bash); got != "bash script\n" {
		t.Errorf("bash completion = %q", got)
	}
}

func TestSandboxedGeneratorCannotReadCredentials(t *testing.T) {
	if _, err := os.Stat(sandboxExec); err != nil {
		t.Skip("needs sandbox-exec")
	}
	ti := newTestInstall(t, appTarball)
	home := t.TempDir()
	ti.in.Where.Home = home
	writeFile(t, filepath.Join(home, ".ssh/id"), []byte("secret"), 0o600)
	script := filepath.Join(t.TempDir(), "gen")
	// Like 1Password's op, it keeps a config under its home.
	writeFile(t, script, []byte("#!/bin/sh\nif cat \"$HOME_SSH/id\" 2>/dev/null; then exit 0; fi\n"+
		"mkdir -p \"$HOME/.config/gen\" || exit 1\necho \"$SHELL ok\"\n"), 0o755)
	t.Setenv("HOME_SSH", filepath.Join(home, ".ssh"))
	artifacts := `[{"binary":["tool"]},{"generate_completions_from_executable":["` + script + `",{"shell_parameter_format":"none","shells":["bash"]}]}]`
	if err := ti.in.Install(context.Background(), ti.cask(t, artifacts)); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, filepath.Join(ti.prefix, "etc/bash_completion.d/gen")); got != "bash ok\n" {
		t.Errorf("completion = %q: the generator read the key or did not run", got)
	}
}
