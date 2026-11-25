package cask

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/sudo"
)

const execBit = 0o111

type outputMode int

const (
	outputOff outputMode = iota
	outputOn
	outputDebug
)

type installerStep struct {
	manual            bool
	path              string
	args              []string
	input             string
	mustSucceed, sudo bool
	stderr            outputMode
	ignored           []string
	overridden        bool
}

var scriptKeys = []string{"args", "input", "executable", "must_succeed", "sudo", "print_stdout", "print_stderr"}

func readInstallers(token string, args json.RawMessage) ([]installerStep, error) {
	invalid := func(format string, a ...any) error {
		return brewError("Cask '%s' definition is invalid: %s", token, fmt.Sprintf(format, a...))
	}
	var list []map[string]json.RawMessage
	if err := json.Unmarshal(args, &list); err != nil {
		return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
	}
	var steps []installerStep
	for _, el := range list {
		if len(el) != 1 {
			return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
		}
		if raw, ok := el["manual"]; ok {
			var path string
			if json.Unmarshal(raw, &path) != nil {
				return nil, invalid("installer requires a path for :manual")
			}
			steps = append(steps, installerStep{manual: true, path: path})
			continue
		}
		raw, ok := el["script"]
		if !ok {
			return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
		}
		s := installerStep{mustSucceed: true, stderr: outputOn}
		var fields map[string]json.RawMessage
		var executable string
		if json.Unmarshal(raw, &executable) == nil {
			s.path = executable
			steps = append(steps, s)
			continue
		}
		if err := json.Unmarshal(raw, &fields); err != nil {
			return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
		}
		keys, err := keyOrder(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
		}
		for _, key := range keys {
			if !slices.Contains(scriptKeys, key) {
				s.ignored = append(s.ignored, key)
			}
			if err := s.set(key, fields[key]); err != nil {
				return nil, fmt.Errorf("%s: installer %s: %w", token, args, ErrUnsupported)
			}
		}
		if s.path == "" {
			return nil, invalid("installer missing executable")
		}
		steps = append(steps, s)
	}
	return steps, nil
}

func (s *installerStep) set(key string, v json.RawMessage) error {
	switch key {
	case "executable":
		return json.Unmarshal(v, &s.path)
	case "must_succeed":
		return json.Unmarshal(v, &s.mustSucceed)
	case "sudo":
		return json.Unmarshal(v, &s.sudo)
	case "print_stdout":
		s.overridden = true
	case "print_stderr":
		var on bool
		if json.Unmarshal(v, &on) == nil {
			s.stderr = outputOff
			if on {
				s.stderr = outputOn
			}
			return nil
		}
		var mode string
		if json.Unmarshal(v, &mode) != nil || mode != "debug" {
			return fmt.Errorf("print_stderr %s", v)
		}
		s.stderr = outputDebug
	case "args":
		var list []any
		if err := json.Unmarshal(v, &list); err != nil {
			return err
		}
		for _, a := range list {
			s.args = append(s.args, fmt.Sprint(a))
		}
	case "input":
		var one string
		if json.Unmarshal(v, &one) == nil {
			s.input = one
			return nil
		}
		var many []string
		if err := json.Unmarshal(v, &many); err != nil {
			return err
		}
		s.input = strings.Join(many, "")
	}
	return nil
}

func (in *Installer) runInstallers(ctx context.Context, token, staged string, steps []installerStep) error {
	for _, s := range steps {
		if s.manual {
			in.logf("Cask %s only provides a manual installer. To run it and complete the installation:\n  open %s",
				token, sudo.ShellEscape(joinName(staged, s.path)))
			continue
		}
		if len(s.ignored) > 0 {
			in.warnf("Unknown arguments to installer -- %s (ignored). Running `brew update; brew cleanup` will likely fix it.",
				rubySymbols(s.ignored))
		}
		if s.overridden && in.Warn != nil {
			_, _ = fmt.Fprintf(in.Warn, "Error: Some arguments to installer will be ignored -- :%s (overridden).\n", rubySymbols(s.ignored))
		}
		in.logf("==> Running installer script '%s'", s.path)
		exe, err := in.stagedExecutable(staged, s.path)
		if err != nil {
			return fmt.Errorf("%s: %w", token, err)
		}
		path := strings.Join([]string{filepath.Join(in.Where.Prefix, "bin"), filepath.Join(in.Where.Prefix, "sbin"), os.Getenv("PATH")}, ":")
		if err := in.runScript(ctx, s, exe, []string{"PATH=" + path}); err != nil {
			return fmt.Errorf("%s: %w", token, err)
		}
	}
	return nil
}

func (in *Installer) runScript(ctx context.Context, s installerStep, exe string, env []string) error {
	c := in.command(sudo.User, exe, s.args...)
	c.Input, c.Env, c.MustSucceed, c.Stdout = s.input, env, s.mustSucceed, in.Log
	if s.sudo {
		c.Privilege = sudo.Sudo
	}
	c.Stderr = nil
	if s.stderr == outputOn || s.stderr == outputDebug && sudo.EnvTrue("HOMEBREW_DEBUG") {
		c.Stderr = in.Warn
	}
	_, err := in.runner()(ctx, c)
	return err
}

// command is SystemCommand.run! with its defaults: it must succeed and prints stderr but not stdout.
func (in *Installer) command(privilege sudo.Privilege, exe string, args ...string) sudo.Command {
	c := sudo.Command{
		Exe: exe, Args: args, Privilege: privilege, MustSucceed: true,
		Stderr: in.Warn, Secrets: sudo.SensitiveValues(), Color: sudo.ColorEnabled(in.Log),
	}
	if sudo.EnvTrue("HOMEBREW_DEBUG") && (in.Verbose || sudo.EnvTrue("HOMEBREW_VERBOSE")) {
		c.Trace = in.Warn
	}
	return c
}

func (in *Installer) capture(ctx context.Context, c sudo.Command) (string, error) {
	res, err := in.runner()(ctx, c)
	return res.Stdout, err
}

func (in *Installer) runner() sudo.Runner {
	if in.Run != nil {
		return in.Run
	}
	return sudo.Exec
}

func (in *Installer) stagedExecutable(staged, path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		path = filepath.Join(in.Where.Home, path[1:])
	}
	abs := path
	if !filepath.IsAbs(path) {
		abs = filepath.Join(staged, path)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return path, nil
	}
	if syscall.Access(abs, 1) != nil {
		if err := in.chmodAll(abs, fi.Mode().Perm()|execBit); err != nil {
			return "", err
		}
	}
	return abs, nil
}

func (in *Installer) chmodAll(path string, perm os.FileMode) error {
	if in.inside(in.Where.Prefix, path) {
		rel, err := filepath.Rel(in.Where.Prefix, path)
		if err != nil {
			return err
		}
		return in.Root.Chmod(rel, perm)
	}
	return os.Chmod(path, perm) //fsx:allow brew makes an installer script outside the prefix executable
}

// sudoArtifact names the first artifact brew installs that requires sudo, or "" if none does.
func (p installPlan) sudoArtifact() string {
	switch {
	case slices.ContainsFunc(p.installers, func(s installerStep) bool { return s.sudo }):
		return "installer"
	case len(p.pkgs) > 0:
		return "pkg"
	}
	return ""
}
