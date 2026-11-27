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
	Err     io.Writer

	// Cask steps have their own bases in place of a formula's.
	Token  string
	Staged string // absolute
	Arch   string // arm or intel
	AppDir string

	Host

	Sandbox       SandboxMode
	SandboxPolicy SandboxPolicy

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

	MatchingCertificate *pathSpec `json:"matching_certificate"`
	Notices             []string  `json:"notices"`
	Name                string    `json:"name"`
	Match               string    `json:"match"`
	FailureMessage      string    `json:"failure_message"`
	Attempts            *int      `json:"attempts"`
	MustSucceed         bool      `json:"must_succeed"`

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

	WritablePaths []pathSpec `json:"writable_paths"`
	NetworkAccess bool       `json:"network_access"`

	Before        string `json:"before"`
	After         string `json:"after"`
	Regexp        bool   `json:"regexp"`
	RegexpOptions int    `json:"regexp_options"`
	SkipAudit     bool   `json:"skip_audit"`
	FirstOnly     bool   `json:"first_only"`
	User          string `json:"user"`
	Group         string `json:"group"`
	ID            string `json:"id"`
	ResolveSource bool   `json:"resolve_source"`
	AbiVersion    string `json:"abi_version"`
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

// RequiresSudo mirrors brew's sudo_required? minus optional steps.
func RequiresSudo(steps []json.RawMessage) bool {
	for _, raw := range steps {
		var s step
		if json.Unmarshal(raw, &s) == nil && s.requiresSudo(false) {
			return true
		}
	}
	return false
}

var supported = []string{
	"mkdir", "mkdir_p", "touch", "symlink", "copy", "move", "remove", "link_dir", "link_children",
	"write", "warn", "set_permissions", "run", "init_data_dir", "terminate_process", "delete_keychain_certificate",
	"set_ownership", "inreplace", "change_dylib_id", "move_children", "move_contents", "install_gzipped_executable",
	"configure_gcc_runtime", "configure_glibc_runtime", "configure_clang_system", "configure_php",
	"bootstrap_cpython", "bootstrap_pypy", "compile_gsettings_schemas", "gio_querymodules",
	"gdk_pixbuf_query_loaders", "gtk_update_icon_cache", "update_mime_database", "update_desktop_database",
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

func (r *runner) uninstallLink(s step) error {
	if s.Target == nil {
		return errors.New("the step has no path")
	}
	abs, err := r.resolve(*s.Target)
	if err != nil {
		return err
	}
	want := r.expand(s.Source.Path)
	if s.Source.Base != "relative" {
		if want, err = r.resolve(*s.Source); err != nil {
			return err
		}
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
	switch name {
	case "home":
		if c.Home == "" {
			return "", errors.New("the home directory is not known")
		}
		return c.Home, nil
	case "temp":
		if c.Temp == "" {
			return "", errors.New("the temporary directory is not known")
		}
		return c.Temp, nil
	}
	if c.cask() {
		switch name {
		case "homebrew_prefix":
			return c.Prefix, nil
		case "staged_path":
			return c.Staged, nil
		case "caskroom_path":
			return filepath.Join(c.Prefix, "Caskroom", c.Token), nil
		}
		if dir, ok := r.configDir(name); ok {
			return dir, nil
		}
		return "", fmt.Errorf("unknown install step base: %s", name)
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
	return "", fmt.Errorf("unknown install step base: %s", name)
}

func (r *runner) expandPath(p string) (string, error) {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, err := r.base("home", "")
		if err != nil {
			return "", err
		}
		return filepath.Join(home, p[1:]), nil
	}
	return filepath.Abs(p)
}

func (r *runner) tempDir() string { return r.c.Temp }

func (r *runner) configDir(name string) (string, bool) {
	prefix := r.c.Prefix
	switch name {
	case "appdir":
		return r.c.AppDir, r.c.AppDir != ""
	case "binarydir":
		return filepath.Join(prefix, "bin"), true
	case "manpagedir":
		return filepath.Join(prefix, "share/man"), true
	case "bash_completion":
		return filepath.Join(prefix, "etc/bash_completion.d"), true
	case "zsh_completion":
		return filepath.Join(prefix, "share/zsh/site-functions"), true
	case "fish_completion":
		return filepath.Join(prefix, "share/fish/vendor_completions.d"), true
	case "pwsh_completion":
		return filepath.Join(prefix, "share/pwsh/completions"), true
	}
	home, err := r.base("home", "")
	if err != nil {
		return "", false
	}
	sub, ok := defaultDirs[name]
	return filepath.Join(home, sub), ok
}

var defaultDirs = map[string]string{
	"appimagedir":          "Applications",
	"colorpickerdir":       "Library/ColorPickers",
	"prefpanedir":          "Library/PreferencePanes",
	"qlplugindir":          "Library/QuickLook",
	"mdimporterdir":        "Library/Spotlight",
	"dictionarydir":        "Library/Dictionaries",
	"fontdir":              "Library/Fonts",
	"servicedir":           "Library/Services",
	"input_methoddir":      "Library/Input Methods",
	"internet_plugindir":   "Library/Internet Plug-Ins",
	"audio_unit_plugindir": "Library/Audio/Plug-Ins/Components",
	"vst_plugindir":        "Library/Audio/Plug-Ins/VST",
	"vst3_plugindir":       "Library/Audio/Plug-Ins/VST3",
	"screen_saverdir":      "Library/Screen Savers",
}

func (r *runner) resolve(s pathSpec) (string, error) {
	p := r.expand(s.Path)
	switch s.Base {
	case "", "absolute":
		return r.expandPath(p)
	case "relative":
		return filepath.Abs(p)
	}
	root, err := r.base(s.Base, s.Formula)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, p), nil
}

func (r *runner) target(s *pathSpec) (string, error) {
	if s == nil {
		return "", errors.New("the step has no path")
	}
	return r.resolve(*s)
}

func (r *runner) logf(format string, args ...any) {
	if r.c.Log != nil {
		_, _ = fmt.Fprintf(r.c.Log, format+"\n", args...)
	}
}

func (r *runner) do(s step) error {
	switch s.Type {
	case "mkdir_p":
		abs, err := r.target(s.Path)
		if err != nil {
			return err
		}
		return r.mkdirAll(abs)
	case "mkdir":
		abs, err := r.target(s.Path)
		if err != nil {
			return err
		}
		return r.mkdir(abs)
	case "touch":
		abs, err := r.target(s.Path)
		if err != nil {
			return err
		}
		return r.touchAt(abs)
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
	case "terminate_process":
		return r.terminateProcess(s)
	case "delete_keychain_certificate":
		return r.deleteKeychainCertificate(s)
	case "set_ownership":
		return r.setOwnership(s)
	case "inreplace":
		return r.inreplace(s)
	case "change_dylib_id":
		return r.changeDylibID(s)
	case "move_children", "move_contents":
		return r.moveContents(s)
	case "install_gzipped_executable":
		return r.installGzippedExecutable(s)
	case "configure_gcc_runtime":
		return r.configureGCCRuntime()
	case "configure_glibc_runtime":
		return r.configureGlibcRuntime()
	case "configure_clang_system":
		return r.configureClangSystem()
	case "configure_php":
		return r.configurePHP()
	case "bootstrap_cpython":
		return r.bootstrapCPython()
	case "bootstrap_pypy":
		return r.bootstrapPyPy(s.AbiVersion)
	case "compile_gsettings_schemas":
		return r.pathTool(s, "glib", "glib-compile-schemas")
	case "gio_querymodules":
		return r.pathTool(s, "glib", "gio-querymodules")
	case "update_mime_database":
		return r.pathTool(s, "shared-mime-info", "update-mime-database")
	case "update_desktop_database":
		return r.pathTool(s, "desktop-file-utils", "update-desktop-database")
	case "gdk_pixbuf_query_loaders":
		return r.formulaTool("gdk-pixbuf", "gdk-pixbuf-query-loaders", "--update-cache")
	case "gtk_update_icon_cache":
		dir, err := r.target(s.Path)
		if err != nil {
			return err
		}
		if r.anyVersionInstalled("gtk4") {
			return r.formulaTool("gtk4", "gtk4-update-icon-cache", "-q", "-t", "-f", dir)
		}
		return r.formulaTool("gtk+3", "gtk3-update-icon-cache", "-q", "-t", "-f", dir)
	}
	return fmt.Errorf("unsupported step %q", s.Type)
}

func (r *runner) remove(s step) error {
	for _, spec := range s.Paths {
		matches, err := r.glob(spec)
		if err != nil {
			return err
		}
		for _, abs := range matches {
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
	dest, err := r.target(s.Target)
	if err != nil {
		return err
	}
	dest = destination(source, dest)
	if err := r.mkdirAll(filepath.Dir(dest)); err != nil {
		return err
	}
	_, existsErr := os.Lstat(dest)
	exists := existsErr == nil
	overwrite := s.Overwrite == nil || *s.Overwrite
	if exists && !overwrite {
		return fmt.Errorf("%s already exists", dest)
	}
	if exists {
		if s.Recursive {
			if err := r.removeTree(dest); err != nil {
				return err
			}
		} else if err := r.removeAny(dest); err != nil {
			return err
		}
	}
	if s.Recursive {
		return r.copyTree(source, dest)
	}
	return r.copyFile(source, dest)
}

func (r *runner) move(s step) error {
	source, err := r.sourceFor(s)
	if err != nil {
		return err
	}
	dest, err := r.target(s.Target)
	if err != nil {
		return err
	}
	dest = destination(source, dest)
	if err := r.mkdirAll(filepath.Dir(dest)); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		if s.Overwrite != nil && !*s.Overwrite {
			return fmt.Errorf("%s already exists", dest)
		}
		if err := r.removeTree(dest); err != nil {
			return err
		}
	}
	return r.rename(source, dest)
}

func (r *runner) linkDir(s step) error {
	source, err := r.resolve(*s.Source)
	if err != nil {
		return err
	}
	targetAbs, err := r.target(s.Target)
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
		if fi, err := os.Lstat(dst); err == nil && fi.IsDir() {
			return nil
		}
		if err := r.removeAny(dst); err != nil {
			return err
		}
		if d.IsDir() {
			return r.mkdirAll(dst)
		}
		return r.relativeLink(p, dst)
	})
}

func (r *runner) linkChildren(s step) error {
	source, err := r.resolve(*s.Source)
	if err != nil {
		return err
	}
	targetAbs, err := r.target(s.Target)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(targetAbs); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, e := range entries {
		dst := filepath.Join(targetAbs, r.expand(s.Prefix)+e.Name()+r.expand(s.Suffix))
		if err := r.relativeLink(filepath.Join(source, e.Name()), dst); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) relativeLink(source, dest string) error {
	text, err := filepath.Rel(filepath.Dir(dest), source)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(dest)); err != nil {
		return err
	}
	return r.symlinkAt(text, dest)
}

func (r *runner) write(s step) error {
	abs, err := r.target(s.Path)
	if err != nil {
		return err
	}
	_, existsErr := os.Lstat(abs)
	exists := existsErr == nil
	if exists && (s.Overwrite == nil || !*s.Overwrite) {
		return nil
	}
	if err := r.mkdirAll(filepath.Dir(abs)); err != nil {
		return err
	}
	if exists {
		if err := r.removeAny(abs); err != nil {
			return err
		}
	}
	return r.writeFile(abs, []byte(r.expand(s.Content)), fileMode)
}
