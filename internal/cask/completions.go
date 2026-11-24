package cask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var sandboxExec = "/usr/bin/sandbox-exec"

var secretName = regexp.MustCompile(`(?i)(cookie|key|token|password|passphrase|auth)`)

type generator struct {
	commands []string
	baseName string
	format   string
	shells   []string
}

var completionShells = []string{"bash", "zsh", "fish", "pwsh"}

func readGenerator(args json.RawMessage) (generator, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(args, &list); err != nil {
		return generator{}, err
	}
	var g generator
	var opts struct {
		BaseName *string  `json:"base_name"`
		Format   *string  `json:"shell_parameter_format"`
		Shells   []string `json:"shells"`
	}
	for _, raw := range list {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			g.commands = append(g.commands, s)
			continue
		}
		if err := json.Unmarshal(raw, &opts); err != nil {
			return generator{}, err
		}
	}
	if len(g.commands) == 0 {
		return generator{}, errors.New("no command")
	}
	if opts.BaseName != nil {
		g.baseName = *opts.BaseName
	}
	if opts.Format != nil {
		g.format = *opts.Format
	}
	g.shells = opts.Shells
	if g.shells == nil {
		g.shells = []string{"bash", "zsh", "fish"}
		if g.format == "cobra" || g.format == "typer" {
			g.shells = completionShells
		}
	}
	for _, s := range g.shells {
		if !slices.Contains(completionShells, s) {
			return generator{}, fmt.Errorf("shell %s: %w", s, ErrUnsupported)
		}
	}
	return g, nil
}

func (g generator) name(token string) string {
	if g.baseName != "" {
		return g.baseName
	}
	if n := filepath.Base(g.commands[0]); n != "" && n != "." && n != string(filepath.Separator) {
		return n
	}
	return token
}

func completionPath(shell, name string) string {
	dir, file := completionName(shell, name)
	return filepath.Join(dir, file)
}

func trimExt(name string) string {
	if bare := strings.TrimSuffix(name, filepath.Ext(name)); bare != "" {
		return bare
	}
	return name
}

func completionName(shell, name string) (dir, file string) {
	bare := trimExt(name)
	switch shell {
	case "bash":
		return "etc/bash_completion.d", bare
	case "zsh":
		if !strings.HasPrefix(name, "_") {
			name = "_" + bare
		}
		return "share/zsh/site-functions", name
	case "fish":
		if !strings.HasSuffix(name, ".fish") {
			name = bare + ".fish"
		}
		return "share/fish/vendor_completions.d", name
	default:
		if !strings.HasPrefix(name, "_") || !strings.HasSuffix(name, ".ps1") {
			name = "_" + bare + ".ps1"
		}
		return "share/pwsh/completions", name
	}
}

func shellParameter(format, shell, executable string) (args, env []string) {
	param := shell
	if shell == "pwsh" {
		param = "powershell" // what cobra and clap call it
	}
	switch format {
	case "":
		return []string{param}, nil
	case "arg":
		return []string{"--shell=" + param}, nil
	case "clap":
		return nil, []string{"COMPLETE=" + param}
	case "click":
		prog := strings.ReplaceAll(strings.ToUpper(filepath.Base(executable)), "-", "_")
		return nil, []string{"_" + prog + "_COMPLETE=" + param + "_source"}
	case "cobra":
		return []string{"completion", param}, nil
	case "flag":
		return []string{"--" + param}, nil
	case "none":
		return nil, nil
	case "typer":
		return []string{"--show-completion", param}, []string{"_TYPER_COMPLETE_TEST_DISABLE_SHELL_DETECTION=1"}
	default:
		return []string{format + shell}, nil
	}
}

func (in *Installer) generateCompletions(ctx context.Context, token, staged string, g generator, undo *[]func() error) error {
	exe := g.commands[0]
	if !filepath.IsAbs(exe) {
		if real, err := filepath.EvalSymlinks(filepath.Join(staged, exe)); err == nil {
			exe = filepath.Join(staged, exe)
			if err := in.makeExecutable(real); err != nil {
				return err
			}
		}
	}
	for _, shell := range g.shells {
		params, env := shellParameter(g.format, shell, exe)
		argv := append([]string{"SHELL=" + shell}, env...)
		argv = append(append(append(argv, exe), g.commands[1:]...), params...)
		out, err := in.sandboxed(ctx, argv)
		if err != nil {
			in.warnf("Failed to generate %s completions from %s: %v", shell, exe, err)
			continue
		}
		rel := completionPath(shell, g.name(token))
		if err := in.Root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			return err
		}
		if err := removeIfThere(in.Root, rel); err != nil {
			return err
		}
		if err := in.Root.WriteFile(rel, out, 0o644); err != nil {
			return err
		}
		*undo = append(*undo, func() error { return removeIfThere(in.Root, rel) })
	}
	return nil
}

func (in *Installer) removeCompletions(token string, g generator) {
	for _, shell := range g.shells {
		if err := removeIfThere(in.Root, completionPath(shell, g.name(token))); err != nil {
			in.warnf("Failed to remove %s generated completions: %v", shell, err)
		}
	}
}

func (in *Installer) sandboxed(ctx context.Context, argv []string) ([]byte, error) {
	tmp, err := os.MkdirTemp("", "brw-sandbox") //fsx:allow scratch space outside the prefix
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(tmp) }() //fsx:allow scratch space outside the prefix
	env := []string{"/usr/bin/env"}
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); secretName.MatchString(k) {
			env = append(env, "-u", k)
		}
	}
	env = append(env, "-C", tmp, "HOME="+tmp, "TMPDIR="+tmp, "TEMP="+tmp, "TMP="+tmp)
	argv = append(env, argv...)
	if fi, err := os.Stat(sandboxExec); err != nil || fi.Mode()&0o111 == 0 {
		if !in.warnedSandbox {
			in.warnf("Sandbox unavailable: running cask artifact operations without sandboxing!")
			in.warnedSandbox = true
		}
		return in.exec(ctx, argv[0], argv[1:]...)
	}
	profile := sandboxProfile(in.Where.Home, []string{in.Where.Prefix, in.Where.AppDir})
	return in.exec(ctx, sandboxExec, append([]string{"-p", profile}, argv...)...)
}

var personalPaths = []string{
	".ssh", ".gitconfig", ".git-credentials", ".config/gh", ".netrc",
	".aws", ".azure", ".boto", ".docker", ".config/fish", ".config/gcloud", ".config/huggingface",
	".config/pip", ".config/pypoetry", ".config/rclone", ".config/containers/auth.json",
	".config/composer/auth.json", ".config/sops/age/keys.txt", ".gnupg", ".gsutil", ".kube",
	".npmrc", ".yarnrc", ".yarnrc.yml", ".pnpmrc", ".bunfig.toml", ".pypirc", ".pip", ".poetry",
	".local/share/pypoetry", ".gem/credentials", ".bundle/config", ".cargo/credentials",
	".cargo/credentials.toml", ".composer/auth.json", ".condarc", ".m2/settings.xml",
	".gradle/gradle.properties", ".sbt/1.0/credentials.sbt", ".terraform.d/credentials.tfrc.json",
	".pulumi/credentials.json", ".oci/config", ".huggingface/token", ".cache/huggingface/token",
	".claude", ".claude.json", ".kiro", ".bash_login", ".bash_logout", ".bash_profile", ".bashrc",
	".bash_history", ".profile", ".zlogin", ".zlogout", ".zprofile", ".zshenv", ".zshrc",
	".zsh_history", ".python_history", ".mysql_history", ".psql_history", ".env", ".env.local",
	"Documents", "Movies", "Music", "Pictures", "Library/Keychains", "Library/Mobile Documents",
	"Library/CloudStorage", "Dropbox", "Google Drive", "OneDrive",
}

func sandboxProfile(home string, needed []string) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n(deny network*)\n(deny file-write*)\n")
	b.WriteString(`(allow file-write* (subpath "/private/tmp") (subpath "/private/var/tmp")` +
		` (regex #"^/private/var/folders/[^/]+/[^/]+/[C,T]/")` +
		` (literal "/dev/null") (literal "/dev/zero") (literal "/dev/random") (literal "/dev/dtracehelper")` +
		` (literal "/dev/ptmx") (regex #"^/dev/fd/[0-9]+$") (regex #"^/dev/tty[a-z0-9]*$"))` + "\n")
	if home == "" {
		return b.String()
	}
	realHome := resolved(home)
	needed = slices.Clone(needed)
	for i := range needed {
		needed[i] = resolvedPath(needed[i])
	}
	for _, p := range personalPaths {
		path, err := filepath.EvalSymlinks(filepath.Join(home, p))
		if err != nil || !within(realHome, path) {
			continue
		}
		if slices.ContainsFunc(needed, func(n string) bool { return within(path, n) }) {
			continue
		}
		fmt.Fprintf(&b, "(deny file-read* (subpath %s))\n", sbplString(path))
	}
	return b.String()
}

func resolvedPath(path string) string {
	rest := ""
	for p := path; ; p = filepath.Dir(p) {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return filepath.Join(r, rest)
		}
		if p == filepath.Dir(p) {
			return path
		}
		rest = filepath.Join(filepath.Base(p), rest)
	}
}

func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
