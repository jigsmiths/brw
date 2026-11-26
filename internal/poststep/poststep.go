// Package poststep runs the post_install steps that the formula API lists,
// with the meaning brew gives them.
package poststep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/sudo"
)

type Context struct {
	Root    *fsx.Root
	Prefix  string // absolute
	Name    string
	Version string // without its revision
	Keg     string // relative to the prefix
	OS      string // darwin or linux
	User    string
	Log     io.Writer

	// Cask steps have their own bases in place of a formula's.
	Token  string
	Staged string // absolute
	Arch   string // arm or intel
	AppDir string

	Sudo   sudo.Runner
	Remove func(ctx context.Context, path string) error
}

func (c Context) cask() bool { return c.Token != "" }

type pathSpec struct {
	Base    string `json:"base"`
	Path    string `json:"path"`
	Formula string `json:"formula"`
}

type guard struct {
	pathSpec
	Condition string `json:"condition"`
	Value     string `json:"value"`
	ID        string `json:"id"`
}

type step struct {
	Type   string  `json:"type"`
	Guards []guard `json:"guards"`

	Path       *pathSpec  `json:"path"`
	Source     *pathSpec  `json:"source"`
	Target     *pathSpec  `json:"target"`
	Command    *pathSpec  `json:"command"`
	StdinPath  *pathSpec  `json:"stdin_path"`
	StdoutPath *pathSpec  `json:"stdout_path"`
	Chdir      *pathSpec  `json:"chdir"`
	Paths      []pathSpec `json:"paths"`

	Args []string          `json:"args"`
	Env  map[string]string `json:"env"`

	Permissions           string `json:"permissions"`
	Content               string `json:"content"`
	Message               string `json:"message"`
	Using                 string `json:"using"`
	Locale                string `json:"locale"`
	Prefix                string `json:"prefix"`
	Suffix                string `json:"suffix"`
	SymlinkTargetContains string `json:"symlink_target_contains"`
	ContentContains       string `json:"content_contains"`

	Sudo sudoMode `json:"sudo"`

	Force          bool  `json:"force"`
	Recursive      bool  `json:"recursive"`
	NonRecursive   bool  `json:"non_recursive"`
	SourceGlob     bool  `json:"source_glob"`
	AllowFailure   bool  `json:"allow_failure"`
	SuppressStderr bool  `json:"suppress_stderr"`
	PrintStdout    bool  `json:"print_stdout"`
	Overwrite      *bool `json:"overwrite"`
	Uninstall      bool  `json:"uninstall"`
}

type sudoMode int

const (
	sudoNever sudoMode = iota
	sudoAlways
	sudoIfNeeded
)

// UnmarshalJSON reads true and "if_needed"; brew treats any other value as no sudo.
func (m *sudoMode) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch v {
	case true:
		*m = sudoAlways
	case "if_needed":
		*m = sudoIfNeeded
	}
	return nil
}

// RequiresSudo is brew's sudo_required? without the optional steps: only `sudo: true` counts.
func RequiresSudo(steps []json.RawMessage) bool {
	for _, raw := range steps {
		var s step
		if json.Unmarshal(raw, &s) == nil && s.Sudo == sudoAlways {
			return true
		}
	}
	return false
}

var supported = []string{
	"mkdir", "mkdir_p", "touch", "symlink", "copy", "move", "remove", "link_dir", "link_children",
	"write", "warn", "set_permissions", "run", "init_data_dir",
}

// Unsupported has no repeats.
func Unsupported(steps []json.RawMessage) []string {
	var out []string
	for _, raw := range steps {
		var s struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(raw, &s); err != nil {
			s.Type = "unreadable step"
		}
		if !slices.Contains(supported, s.Type) && !slices.Contains(out, s.Type) {
			out = append(out, s.Type)
		}
	}
	return out
}

// Run runs nothing if any step is unsupported.
func Run(ctx context.Context, c Context, raw []json.RawMessage) error {
	if u := Unsupported(raw); len(u) > 0 {
		return fmt.Errorf("unsupported post_install steps: %s", strings.Join(u, ", "))
	}
	r := &runner{ctx: ctx, c: c, guards: map[string]bool{}}
	for i, b := range raw {
		var s step
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("post_install step %d: %w", i+1, err)
		}
		if !r.guardsMatch(s) {
			continue
		}
		if err := r.do(s); err != nil {
			return fmt.Errorf("post_install step %d (%s): %w", i+1, s.Type, err)
		}
	}
	return nil
}

// Uninstall removes only the symlinks of steps marked uninstall, and only if they
// still lead where the step pointed them.
func Uninstall(ctx context.Context, c Context, raw []json.RawMessage) error {
	r := &runner{ctx: ctx, c: c, guards: map[string]bool{}}
	for i, b := range raw {
		var s step
		if err := json.Unmarshal(b, &s); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
		if s.Type != "symlink" || !s.Uninstall || s.Source == nil {
			continue
		}
		if err := r.uninstallLink(s); err != nil {
			return fmt.Errorf("step %d: %w", i+1, err)
		}
	}
	return nil
}

func (r *runner) uninstallLink(s step) error {
	if s.Target == nil {
		return errors.New("the step has no path")
	}
	abs, err := r.resolve(*s.Target)
	if err != nil {
		return err
	}
	rel, err := r.inside(abs)
	if err != nil && s.Sudo == sudoNever {
		return err
	}
	want := r.expand(s.Source.Path)
	if s.Source.Base != "relative" {
		if want, err = r.resolve(*s.Source); err != nil {
			return err
		}
	}
	if s.Sudo == sudoNever {
		if got, err := r.c.Root.Readlink(rel); err != nil || got != want {
			return nil
		}
		return r.c.Root.Remove(rel)
	}
	if fi, err := os.Lstat(abs); err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return nil
	}
	got, err := r.readlink(s, abs)
	if err != nil || got != want {
		return nil
	}
	if r.needsSudo(s, filepath.Dir(abs)) {
		if r.c.Remove == nil {
			return errors.New("no way to remove a path with sudo")
		}
		return r.c.Remove(r.ctx, abs)
	}
	return r.removeAny(abs)
}

type runner struct {
	ctx    context.Context
	c      Context
	guards map[string]bool // answers by guard, kept for the whole run
}

func (r *runner) guardsMatch(s step) bool {
	for _, g := range s.Guards {
		key, _ := json.Marshal(g)
		matches, seen := r.guards[string(key)]
		if !seen {
			matches = r.guardMatches(g)
			r.guards[string(key)] = matches
		}
		if !matches {
			return false
		}
	}
	return true
}

func (r *runner) guardMatches(g guard) bool {
	switch g.Condition {
	case "if_exists", "unless_exists":
		exists := false
		if paths, err := r.glob(g.pathSpec); err == nil {
			for _, p := range paths {
				if _, err := os.Stat(p); err == nil {
					exists = true
				}
			}
		}
		return exists == (g.Condition == "if_exists")
	case "on":
		return (g.Value == "macos" && r.c.OS == "darwin") || (g.Value == "linux" && r.c.OS == "linux")
	}
	return false
}

var template = regexp.MustCompile(`\{\{([A-Za-z_][\w.]*)\}\}`)

// expand leaves unknown {{tokens}} as written.
func (r *runner) expand(s string) string {
	return template.ReplaceAllStringFunc(s, func(m string) string {
		if v, ok := r.token(m[2 : len(m)-2]); ok {
			return v
		}
		return m
	})
}

func (r *runner) token(name string) (string, bool) {
	c := r.c
	if c.cask() {
		switch name {
		case "token", "name":
			return c.Token, true
		case "arch":
			return c.Arch, true
		case "appdir":
			return c.AppDir, c.AppDir != ""
		case "staged_path", "caskroom_path":
			p, err := r.base(name, "")
			return p, err == nil
		case "formula_name", "prefix", "opt_prefix", "bin", "sbin", "lib", "libexec", "share", "pkgshare", "var", "etc",
			"pkgetc", "rack", "bash_completion", "zsh_completion", "fish_completion", "pwsh_completion":
			return "", false
		}
	}
	switch name {
	case "HOMEBREW_PREFIX":
		return c.Prefix, true
	case "HOMEBREW_CELLAR":
		return filepath.Join(c.Prefix, "Cellar"), true
	case "formula_name", "name":
		return c.Name, true
	case "user":
		return c.User, c.User != ""
	case "version":
		return c.Version, c.Version != ""
	case "version.major":
		return strings.SplitN(c.Version, ".", 2)[0], c.Version != ""
	case "version.major_minor":
		parts := strings.SplitN(c.Version, ".", 3)
		return strings.Join(parts[:min(2, len(parts))], "."), c.Version != ""
	}
	switch name {
	case "prefix", "opt_prefix", "bin", "sbin", "lib", "libexec", "share", "pkgshare", "var", "etc", "pkgetc", "rack",
		"bash_completion", "zsh_completion", "fish_completion", "pwsh_completion":
		if p, err := r.base(name, ""); err == nil {
			return p, true
		}
	}
	return "", false
}

func (r *runner) base(name, formula string) (string, error) {
	c := r.c
	if c.cask() {
		switch name {
		case "homebrew_prefix":
			return c.Prefix, nil
		case "staged_path":
			return c.Staged, nil
		case "caskroom_path":
			return filepath.Join(c.Prefix, "Caskroom", c.Token), nil
		}
		return "", fmt.Errorf("unsupported base %q for a cask", name)
	}
	keg := filepath.Join(c.Prefix, c.Keg)
	switch name {
	case "homebrew_prefix":
		return c.Prefix, nil
	case "var", "etc":
		return filepath.Join(c.Prefix, name), nil
	case "pkgetc":
		return filepath.Join(c.Prefix, "etc", c.Name), nil
	case "opt_prefix":
		return filepath.Join(c.Prefix, "opt", c.Name), nil
	case "rack":
		return filepath.Join(c.Prefix, "Cellar", c.Name), nil
	case "prefix":
		return keg, nil
	case "bin", "sbin", "lib", "libexec", "share", "include":
		return filepath.Join(keg, name), nil
	case "pkgshare":
		return filepath.Join(keg, "share", c.Name), nil
	case "frameworks":
		return filepath.Join(keg, "Frameworks"), nil
	case "bash_completion":
		return filepath.Join(keg, "etc", "bash_completion.d"), nil
	case "zsh_completion":
		return filepath.Join(keg, "share", "zsh", "site-functions"), nil
	case "fish_completion":
		return filepath.Join(keg, "share", "fish", "vendor_completions.d"), nil
	case "pwsh_completion":
		return filepath.Join(keg, "share", "pwsh", "completions"), nil
	case "formula_pkgetc":
		return filepath.Join(c.Prefix, "etc", path.Base(formula)), nil
	case "formula_opt_prefix":
		return filepath.Join(c.Prefix, "opt", path.Base(formula)), nil
	}
	return "", fmt.Errorf("unsupported base %q", name)
}

func (r *runner) resolve(s pathSpec) (string, error) {
	p := r.expand(s.Path)
	switch s.Base {
	case "", "absolute":
		if !filepath.IsAbs(p) {
			return "", fmt.Errorf("%q is relative and has no base", p)
		}
		return filepath.Clean(p), nil
	case "relative":
		return "", errors.New("a relative base only works for a symlink source")
	}
	root, err := r.base(s.Base, s.Formula)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, p), nil
}

func (r *runner) inside(abs string) (string, error) {
	rel, err := filepath.Rel(r.c.Prefix, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%s is outside the prefix", abs)
	}
	return rel, nil
}

func (r *runner) target(s *pathSpec) (abs, rel string, err error) {
	if s == nil {
		return "", "", errors.New("the step has no path")
	}
	if abs, err = r.resolve(*s); err != nil {
		return "", "", err
	}
	rel, err = r.inside(abs)
	return abs, rel, err
}

func (r *runner) logf(format string, args ...any) {
	if r.c.Log != nil {
		_, _ = fmt.Fprintf(r.c.Log, format+"\n", args...)
	}
}

func (r *runner) do(s step) error {
	switch s.Type {
	case "mkdir_p":
		_, rel, err := r.target(s.Path)
		if err != nil {
			return err
		}
		return r.c.Root.MkdirAll(rel, 0o755)
	case "mkdir":
		_, rel, err := r.target(s.Path)
		if err != nil {
			return err
		}
		return r.c.Root.Mkdir(rel, 0o755)
	case "touch":
		return r.touch(s)
	case "symlink":
		return r.symlink(s)
	case "remove":
		return r.remove(s)
	case "copy":
		return r.copy(s)
	case "move":
		return r.move(s)
	case "link_dir":
		return r.linkDir(s)
	case "link_children":
		return r.linkChildren(s)
	case "write":
		return r.write(s)
	case "warn":
		r.logf("Warning: %s", r.expand(s.Message))
		return nil
	case "set_permissions":
		return r.setPermissions(s)
	case "run":
		return r.run(s)
	case "init_data_dir":
		return r.initDataDir(s)
	}
	return fmt.Errorf("unsupported step %q", s.Type)
}

func (r *runner) touch(s step) error {
	_, rel, err := r.target(s.Path)
	if err != nil {
		return err
	}
	if err := r.c.Root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	if _, err := r.c.Root.Lstat(rel); err == nil {
		now := time.Now()
		return r.c.Root.Chtimes(rel, now, now)
	}
	return r.c.Root.WriteFile(rel, nil, 0o644)
}

// removeFile ignores a missing file and leaves a directory alone.
func (r *runner) removeFile(rel string) error {
	fi, err := r.c.Root.Lstat(rel)
	if err != nil || fi.IsDir() {
		return nil
	}
	return r.c.Root.Remove(rel)
}

func (r *runner) symlink(s step) error {
	if s.Sudo != sudoNever {
		return r.symlinkWithSudo(s)
	}
	_, rel, err := r.target(s.Target)
	if err != nil {
		return err
	}
	if s.SourceGlob {
		sources, err := r.glob(*s.Source)
		if err != nil {
			return err
		}
		sources = slices.DeleteFunc(sources, func(p string) bool { _, err := os.Lstat(p); return err != nil })
		if len(sources) == 0 {
			return nil
		}
		if fi, err := r.c.Root.Lstat(rel); len(sources) > 1 || (err == nil && fi.IsDir()) {
			if err := r.c.Root.MkdirAll(rel, 0o755); err != nil {
				return err
			}
			for _, src := range sources {
				if err := r.link(src, path.Join(rel, filepath.Base(src)), s.Force); err != nil {
					return err
				}
			}
			return nil
		}
		return r.link(sources[0], rel, s.Force)
	}
	if s.Source == nil {
		return errors.New("the step has no source")
	}
	text := r.expand(s.Source.Path)
	if s.Source.Base != "relative" {
		if text, err = r.resolve(*s.Source); err != nil {
			return err
		}
	}
	return r.link(text, rel, s.Force)
}

func (r *runner) link(sourceText, rel string, force bool) error {
	if force {
		if err := r.removeFile(rel); err != nil {
			return err
		}
	}
	if err := r.c.Root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	return r.c.Root.Symlink(sourceText, rel)
}

func (r *runner) remove(s step) error {
	for _, spec := range s.Paths {
		matches, err := r.glob(spec)
		if err != nil {
			return err
		}
		for _, abs := range matches {
			if s.Sudo == sudoNever {
				if _, err := r.inside(abs); err != nil {
					return err
				}
			}
			fi, err := os.Lstat(abs)
			if err != nil {
				continue
			}
			if s.SymlinkTargetContains != "" {
				target, err := r.readlink(s, abs)
				if fi.Mode()&fs.ModeSymlink == 0 || err != nil || !strings.Contains(target, r.expand(s.SymlinkTargetContains)) {
					continue
				}
			}
			if s.ContentContains != "" {
				target, err := os.Stat(abs)
				if err != nil || !target.Mode().IsRegular() {
					continue
				}
				b, err := os.ReadFile(abs)
				if err != nil || !strings.Contains(string(b), r.expand(s.ContentContains)) {
					continue
				}
			}
			if err := r.removePath(s, abs); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runner) removePath(s step, abs string) error {
	switch {
	case r.needsSudo(s, filepath.Dir(abs)):
		if r.c.Remove == nil {
			return errors.New("no way to remove a path with sudo")
		}
		return r.c.Remove(r.ctx, abs)
	case s.Recursive:
		return r.removeTree(abs)
	}
	return r.removeAny(abs)
}

func destination(source, target string) string {
	if fi, err := os.Stat(target); err == nil && fi.IsDir() {
		return filepath.Join(target, filepath.Base(source))
	}
	return target
}

func (r *runner) sourceFor(s step) (string, error) {
	if s.Source == nil {
		return "", errors.New("the step has no source")
	}
	if !s.SourceGlob {
		return r.resolve(*s.Source)
	}
	matches, err := r.glob(*s.Source)
	if err != nil {
		return "", err
	}
	matches = slices.DeleteFunc(matches, func(p string) bool { _, err := os.Lstat(p); return err != nil })
	if len(matches) != 1 {
		return "", fmt.Errorf("the source glob must match exactly one path, it matched %d", len(matches))
	}
	return matches[0], nil
}

func (r *runner) copy(s step) error {
	source, err := r.sourceFor(s)
	if err != nil {
		return err
	}
	targetAbs, _, err := r.target(s.Target)
	if err != nil {
		return err
	}
	dest := destination(source, targetAbs)
	rel, err := r.inside(dest)
	if err != nil {
		return err
	}
	if err := r.c.Root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	_, existsErr := r.c.Root.Lstat(rel)
	exists := existsErr == nil
	overwrite := s.Overwrite == nil || *s.Overwrite
	if exists && !overwrite {
		return fmt.Errorf("%s already exists", dest)
	}
	if exists {
		if s.Recursive {
			if err := r.c.Root.RemoveAll(rel); err != nil {
				return err
			}
		} else if err := r.removeFile(rel); err != nil {
			return err
		}
	}
	if s.Recursive {
		return r.copyTree(source, rel)
	}
	return r.copyFile(source, rel)
}

func (r *runner) copyFile(source, rel string) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	return r.c.Root.WriteFrom(rel, f, fi.Mode().Perm(), time.Time{})
}

func (r *runner) copyTree(source, rel string) error {
	return filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sub, _ := filepath.Rel(source, p)
		dst := path.Join(rel, filepath.ToSlash(sub))
		switch {
		case d.IsDir():
			return r.c.Root.MkdirAll(dst, 0o755)
		case d.Type()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return r.c.Root.Symlink(target, dst)
		}
		return r.copyFile(p, dst)
	})
}

func (r *runner) move(s step) error {
	source, err := r.sourceFor(s)
	if err != nil {
		return err
	}
	srcRel, err := r.inside(source)
	if err != nil {
		return err
	}
	targetAbs, _, err := r.target(s.Target)
	if err != nil {
		return err
	}
	dest := destination(source, targetAbs)
	rel, err := r.inside(dest)
	if err != nil {
		return err
	}
	if err := r.c.Root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	if _, err := r.c.Root.Lstat(rel); err == nil {
		if s.Overwrite != nil && !*s.Overwrite {
			return fmt.Errorf("%s already exists", dest)
		}
		if err := r.c.Root.RemoveAll(rel); err != nil {
			return err
		}
	}
	return r.c.Root.Rename(srcRel, rel)
}

func (r *runner) linkDir(s step) error {
	source, err := r.resolve(*s.Source)
	if err != nil {
		return err
	}
	targetAbs, _, err := r.target(s.Target)
	if err != nil {
		return err
	}
	return filepath.WalkDir(source, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		sub, _ := filepath.Rel(source, p)
		if d.Name() == ".DS_Store" {
			return nil
		}
		dst := filepath.Join(targetAbs, sub)
		rel, err := r.inside(dst)
		if err != nil {
			return err
		}
		if fi, err := r.c.Root.Lstat(rel); err == nil && fi.IsDir() {
			return nil
		}
		if err := r.removeFile(rel); err != nil {
			return err
		}
		if d.IsDir() {
			return r.c.Root.MkdirAll(rel, 0o755)
		}
		return r.relativeLink(p, rel)
	})
}

func (r *runner) linkChildren(s step) error {
	source, err := r.resolve(*s.Source)
	if err != nil {
		return err
	}
	targetAbs, targetRel, err := r.target(s.Target)
	if err != nil {
		return err
	}
	if err := r.c.Root.MkdirAll(targetRel, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, e := range entries {
		dst := filepath.Join(targetAbs, r.expand(s.Prefix)+e.Name()+r.expand(s.Suffix))
		rel, err := r.inside(dst)
		if err != nil {
			return err
		}
		if err := r.relativeLink(filepath.Join(source, e.Name()), rel); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) relativeLink(source, rel string) error {
	dir := filepath.Join(r.c.Prefix, path.Dir(rel))
	text, err := filepath.Rel(dir, source)
	if err != nil {
		return err
	}
	if err := r.c.Root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	return r.c.Root.Symlink(text, rel)
}

func (r *runner) write(s step) error {
	_, rel, err := r.target(s.Path)
	if err != nil {
		return err
	}
	exists := false
	if _, err := r.c.Root.Lstat(rel); err == nil {
		exists = true
	}
	if exists && (s.Overwrite == nil || !*s.Overwrite) {
		return nil
	}
	if err := r.c.Root.MkdirAll(path.Dir(rel), 0o755); err != nil {
		return err
	}
	if exists {
		if err := r.c.Root.Remove(rel); err != nil {
			return err
		}
	}
	return r.c.Root.WriteFile(rel, []byte(r.expand(s.Content)), 0o644)
}
