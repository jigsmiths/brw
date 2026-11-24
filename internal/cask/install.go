package cask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/platform"
)

// Config is what brew records in config.json; env stays empty because brw does not read HOMEBREW_CASK_OPTS.
type Config struct {
	Languages      []string
	ExplicitAppDir string
}

type Installer struct {
	Root    *fsx.Root
	Apps    *fsx.Root
	Where   Where
	Host    platform.Host
	Fetcher *Fetcher
	Log     io.Writer
	Warn    io.Writer
	Now     func() time.Time
	Sleep   func(time.Duration)
	Version string
	APIPath string
	Config  Config
	Exec    Exec
	Adopt   bool

	upgrading     bool
	reopen        []string
	warnedSandbox bool
}

func (in *Installer) logf(format string, args ...any) {
	if in.Log != nil {
		_, _ = fmt.Fprintf(in.Log, format+"\n", args...)
	}
}

type brewMessage string

func (m brewMessage) Error() string { return string(m) }

func brewError(format string, args ...any) error {
	return brewMessage(fmt.Sprintf(format, args...))
}

func (in *Installer) warnf(format string, args ...any) {
	if in.Warn != nil {
		_, _ = fmt.Fprintf(in.Warn, "Warning: "+format+"\n", args...)
	}
}

func (in *Installer) Install(ctx context.Context, c api.Cask) (err error) {
	p, links, file, err := in.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	in.logf("==> Installing Cask %s", c.Token)
	version := filepath.Join("Caskroom", c.Token, c.Version)
	if _, err := in.Root.Lstat(version); err == nil {
		return fmt.Errorf("%s: %s: %w", c.Token, version, fsx.ErrExists)
	}
	var undo []func() error
	defer func() {
		if err != nil {
			err = errors.Join(err, in.discard(ctx, c, undo))
		}
	}()
	if err := in.installArtifacts(ctx, c, p, links, file, &undo); err != nil {
		return err
	}
	if err := in.writeReceipt(c, p, true); err != nil {
		return err
	}
	in.logf("%s was successfully installed!", badge()+c.Token)
	return nil
}

func (in *Installer) prepare(ctx context.Context, c api.Cask, owned map[string]bool) (installPlan, []linkAction, string, error) {
	if msg := deprecation(c, in.Now()); c.Disabled {
		return installPlan{}, nil, "", brewError("Cask '%s' has been %s", c.Token, msg)
	} else if msg != "" {
		in.warnf("%s has been %s", c.Token, msg)
	}
	if err := checkRequirements(c, in.Host); err != nil {
		return installPlan{}, nil, "", err
	}
	p, err := plan(c, in.Where)
	if err != nil {
		return installPlan{}, nil, "", err
	}
	links, err := in.checkTargets(c, p, owned)
	if err != nil {
		return installPlan{}, nil, "", err
	}
	file, err := in.Fetcher.Fetch(ctx, c)
	if err != nil {
		return installPlan{}, nil, "", err
	}
	return p, links, file, nil
}

func (in *Installer) installArtifacts(ctx context.Context, c api.Cask, p installPlan, links []linkAction, file string, undo *[]func() error) error {
	version := filepath.Join("Caskroom", c.Token, c.Version)
	if err := in.Root.MkdirAll(version, 0o755); err != nil {
		return err
	}
	staged, err := in.Root.Abs(version)
	if err != nil {
		return err
	}
	if err := Extract(ctx, file, staged); err != nil {
		return err
	}
	if err := in.writeCaskfile(c); err != nil {
		return err
	}
	if err := in.runSteps("preflight_steps", c.Token, c.Version, staged, p.preflight); err != nil {
		return err
	}
	for _, m := range p.apps {
		if err := in.moveApp(ctx, version, m, c.AutoUpdates, undo); err != nil {
			return fmt.Errorf("%s: %w", c.Token, err)
		}
	}
	for i, m := range p.links {
		if links[i] == skipLink {
			continue
		}
		action, _, err := in.binaryAction(c, m)
		if err != nil {
			return err
		}
		switch action {
		case alreadyLinked:
			in.logf("==> %s '%s' is already linked to '%s'", linkNames[m.kind], filepath.Base(m.source), m.target)
			continue
		case skipLink:
			continue
		}
		if err := in.linkBinary(ctx, staged, m, action, undo); err != nil {
			return fmt.Errorf("%s: %w", c.Token, err)
		}
	}
	for _, g := range p.generators {
		if err := in.generateCompletions(ctx, c.Token, staged, g, undo); err != nil {
			return fmt.Errorf("%s: %w", c.Token, err)
		}
	}
	if err := in.runSteps("postflight_steps", c.Token, c.Version, staged, p.postflight); err != nil {
		return err
	}
	return in.writeConfig(c)
}

func (in *Installer) discard(ctx context.Context, c api.Cask, undo []func() error) error {
	in.logf("==> Purging files for version %s of Cask %s", c.Version, c.Token)
	var err error
	for _, u := range slices.Backward(undo) {
		err = errors.Join(err, u())
	}
	return errors.Join(err, in.purge(context.WithoutCancel(ctx), c))
}

func badge() string {
	if os.Getenv("HOMEBREW_NO_EMOJI") != "" {
		return ""
	}
	if b := os.Getenv("HOMEBREW_INSTALL_BADGE"); b != "" {
		return b + "  "
	}
	return "🍺  "
}

type linkAction int

const (
	newLink linkAction = iota
	overwrite
	alreadyLinked
	skipLink
)

func (in *Installer) checkTargets(c api.Cask, p installPlan, owned map[string]bool) ([]linkAction, error) {
	for _, m := range p.apps {
		if _, err := in.appRel(m.target); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Token, err)
		}
		if owned[m.target] {
			continue
		}
		if _, err := os.Lstat(m.target); err == nil && !in.Adopt {
			return nil, brewError("%s: It seems there is already an App at '%s'.", c.Token, m.target)
		}
	}
	links := make([]linkAction, len(p.links))
	for i, m := range p.links {
		if _, err := in.prefixRel(m.target); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Token, err)
		}
		if filepath.IsAbs(m.source) && !in.inside(in.Where.AppDir, m.source) && !in.inside(in.Where.Prefix, m.source) {
			return nil, fmt.Errorf("%s: %s source %s: outside %s and %s: %w", c.Token, m.kind, m.source, in.Where.AppDir, in.Where.Prefix, ErrUnsupported)
		}
		if owned[m.target] {
			continue
		}
		action, formula, err := in.binaryAction(c, m)
		if err != nil {
			return nil, err
		}
		if action == skipLink {
			in.warnf("It seems there is already a %s at '%s' from formula %s; skipping link.", linkNames[m.kind], m.target, formula)
		}
		links[i] = action
	}
	return links, nil
}

func (in *Installer) binaryAction(c api.Cask, m move) (linkAction, string, error) {
	fi, err := os.Lstat(m.target)
	if errors.Is(err, fs.ErrNotExist) {
		return newLink, "", nil
	}
	if err != nil {
		return 0, "", err
	}
	if fi.Mode()&fs.ModeSymlink != 0 {
		real, err := filepath.EvalSymlinks(m.target)
		if err != nil {
			return newLink, "", nil
		}
		caskroom := resolved(filepath.Join(in.Where.Prefix, "Caskroom", c.Token))
		toSource := in.linksToSource(c, m, real)
		switch {
		case in.Adopt && (toSource || real != caskroom && in.inside(caskroom, real)):
			return overwrite, "", nil
		case toSource:
			return alreadyLinked, "", nil
		}
		if formula, ok := in.formulaLink(m.target); ok {
			return skipLink, formula, nil
		}
	}
	return 0, "", brewError("%s: It seems there is already a %s at '%s'.", c.Token, linkNames[m.kind], m.target)
}

func (in *Installer) linksToSource(c api.Cask, m move, real string) bool {
	source := m.source
	if !filepath.IsAbs(source) {
		source = filepath.Join(in.Where.Prefix, "Caskroom", c.Token, c.Version, source)
	}
	if link, err := os.Readlink(m.target); err == nil && link == source {
		return true
	}
	resolved, err := filepath.EvalSymlinks(source)
	return err == nil && resolved == real
}

func (in *Installer) formulaLink(path string) (string, bool) {
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", false
	}
	cellar := resolved(filepath.Join(in.Where.Prefix, "Cellar"))
	rel, err := filepath.Rel(cellar, real)
	if err != nil || rel == "." || !in.inside(cellar, real) {
		return "", false
	}
	formula, _, _ := strings.Cut(rel, string(filepath.Separator))
	return formula, true
}

func resolved(dir string) string {
	if r, err := filepath.EvalSymlinks(dir); err == nil {
		return r
	}
	return dir
}

func (in *Installer) inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (in *Installer) appRel(target string) (string, error) {
	if !in.inside(in.Where.AppDir, target) {
		return "", fmt.Errorf("app target %s: outside %s: %w", target, in.Where.AppDir, ErrUnsupported)
	}
	return filepath.Rel(in.Where.AppDir, target)
}

func (in *Installer) prefixRel(target string) (string, error) {
	if !in.inside(in.Where.Prefix, target) {
		return "", fmt.Errorf("binary target %s: outside %s: %w", target, in.Where.Prefix, ErrUnsupported)
	}
	return filepath.Rel(in.Where.Prefix, target)
}

// moveApp keeps an adopted app at the target on rollback too, unlike brew.
func (in *Installer) moveApp(ctx context.Context, version string, m move, autoUpdates bool, undo *[]func() error) error {
	staged := filepath.Join(version, m.source)
	if _, err := in.Root.Lstat(staged); err != nil {
		abs, _ := in.Root.Abs(staged)
		return brewError("It seems the App source '%s' is not there.", abs)
	}
	rel, err := in.appRel(m.target)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(m.target); err == nil && in.Adopt {
		in.logf("==> Adopting existing App at '%s'", m.target)
		if !autoUpdates {
			abs, err := in.Root.Abs(staged)
			if err != nil {
				return err
			}
			if !in.sameApp(ctx, abs, m.target) {
				return brewError("It seems the existing App is different from the one being installed.")
			}
		}
		if err := in.Root.RemoveAll(staged); err != nil {
			return err
		}
		return in.linkApp(ctx, staged, m, undo)
	}
	in.logf("==> Moving App '%s' to '%s'", filepath.Base(m.source), m.target)
	if err := in.mkdirPath(ctx, in.Apps, filepath.Dir(rel)); err != nil {
		return err
	}
	if writable(filepath.Dir(m.target)) {
		if err := in.Root.MoveTo(in.Apps, staged, rel); err != nil {
			return err
		}
		*undo = append(*undo, func() error { return in.Apps.MoveTo(in.Root, rel, staged) })
	} else {
		abs, err := in.Root.Abs(staged)
		if err != nil {
			return err
		}
		if err := in.sudoIfNeeded(ctx, "/bin/cp", "-pR", abs, m.target); err != nil {
			return err
		}
		*undo = append(*undo, func() error { return in.removePath(context.WithoutCancel(ctx), in.Apps, rel) })
		if err := in.removePath(ctx, in.Root, staged); err != nil {
			return err
		}
	}
	return in.linkApp(ctx, staged, m, undo)
}

func (in *Installer) linkApp(ctx context.Context, staged string, m move, undo *[]func() error) error {
	if err := in.Root.SymlinkTo(in.Apps, m.target, staged); err != nil {
		return err
	}
	*undo = append(*undo, func() error { return in.Root.Remove(staged) })
	if err := run(ctx, "chmod", "-R", "a+rX,go-w", m.target); err != nil {
		return err
	}
	if group := caskroomGroup(); group != "" {
		_ = run(ctx, "chgrp", "-hR", group, m.target)
	}
	return nil
}

func caskroomGroup() string {
	u, err := user.Current()
	if err != nil {
		return ""
	}
	if admin, err := user.LookupGroup("admin"); err == nil {
		if ids, err := u.GroupIds(); err == nil && slices.Contains(ids, admin.Gid) {
			return "admin"
		}
	}
	if g, err := user.LookupGroupId(u.Gid); err == nil {
		return g.Name
	}
	return ""
}

func (in *Installer) linkBinary(ctx context.Context, staged string, m move, action linkAction, undo *[]func() error) error {
	source := m.source
	if !filepath.IsAbs(source) {
		source = filepath.Join(staged, source)
	}
	real, err := filepath.EvalSymlinks(source)
	if err != nil {
		return brewError("It seems the symlink source '%s' is not there.", source)
	}
	rel, err := in.prefixRel(m.target)
	if err != nil {
		return err
	}
	if action == overwrite {
		in.warnf("It seems there is already a %s at '%s'; overwriting.", linkNames[m.kind], m.target)
		if err := in.removePath(ctx, in.Root, rel); err != nil {
			return err
		}
	}
	in.logf("==> Linking %s '%s' to '%s'", linkNames[m.kind], filepath.Base(m.source), m.target)
	if err := in.Root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := removeIfThere(in.Root, rel); err != nil {
		return err
	}
	if in.inside(in.Where.AppDir, source) {
		err = in.Root.SymlinkTo(in.Apps, source, rel)
	} else {
		err = in.Root.Symlink(source, rel)
	}
	if err != nil {
		return err
	}
	*undo = append(*undo, func() error { return in.Root.Remove(rel) })
	if m.kind != "binary" {
		return nil
	}
	return in.makeExecutable(real)
}

func (in *Installer) makeExecutable(path string) error {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o111 == 0o111 {
		return err
	}
	perm := fi.Mode().Perm() | 0o111
	for _, r := range []struct {
		dir  string
		root *fsx.Root
	}{{in.Where.AppDir, in.Apps}, {in.Where.Prefix, in.Root}} {
		dir, err := filepath.EvalSymlinks(r.dir)
		if err == nil && in.inside(dir, path) {
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			return r.root.Chmod(rel, perm)
		}
	}
	return fmt.Errorf("binary %s: outside %s and %s", path, in.Where.AppDir, in.Where.Prefix)
}

func (in *Installer) purge(ctx context.Context, c api.Cask) error {
	dir := filepath.Join("Caskroom", c.Token)
	err := errors.Join(
		in.removePath(ctx, in.Root, filepath.Join(dir, c.Version)),
		in.removePath(ctx, in.Root, filepath.Join(dir, ".metadata", c.Version)),
	)
	for _, empty := range []string{filepath.Join(dir, ".metadata"), dir} {
		if entries, rerr := in.Root.ReadDir(empty); rerr == nil && len(entries) == 0 {
			err = errors.Join(err, in.Root.Remove(empty))
		}
	}
	return err
}

func removeIfThere(r *fsx.Root, name string) error {
	if _, err := r.Lstat(name); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return r.RemoveAll(name)
}

func (in *Installer) writeCaskfile(c api.Cask) error {
	stamp := in.Now().UTC().Format("20060102150405.000")
	return in.Root.WriteFile(filepath.Join("Caskroom", c.Token, ".metadata", c.Version, stamp, "Casks", c.Token+".json"), []byte("{}"), 0o644)
}

func (in *Installer) writeMetadata(c api.Cask, name string, data []byte) error {
	path := filepath.Join("Caskroom", c.Token, ".metadata", name)
	if err := removeIfThere(in.Root, path); err != nil {
		return err
	}
	return in.Root.WriteFile(path, data, 0o644)
}

func (in *Installer) writeConfig(c api.Cask) error {
	home := in.Where.Home
	lib := func(p string) string { return filepath.Join(home, "Library", p) }
	type dirs struct {
		Languages          []string `json:"languages"`
		AppDir             string   `json:"appdir"`
		AppImageDir        string   `json:"appimagedir"`
		KeyboardLayoutDir  string   `json:"keyboard_layoutdir"`
		ColorPickerDir     string   `json:"colorpickerdir"`
		PrefPaneDir        string   `json:"prefpanedir"`
		QLPluginDir        string   `json:"qlplugindir"`
		MDImporterDir      string   `json:"mdimporterdir"`
		DictionaryDir      string   `json:"dictionarydir"`
		FontDir            string   `json:"fontdir"`
		ServiceDir         string   `json:"servicedir"`
		InputMethodDir     string   `json:"input_methoddir"`
		InternetPluginDir  string   `json:"internet_plugindir"`
		AudioUnitPluginDir string   `json:"audio_unit_plugindir"`
		VSTPluginDir       string   `json:"vst_plugindir"`
		VST3PluginDir      string   `json:"vst3_plugindir"`
		ScreenSaverDir     string   `json:"screen_saverdir"`
	}
	type appDir struct {
		AppDir string `json:"appdir,omitempty"`
	}
	languages := in.Config.Languages
	if languages == nil {
		languages = []string{}
	}
	b, err := json.Marshal(struct {
		Default  dirs   `json:"default"`
		Env      appDir `json:"env"`
		Explicit appDir `json:"explicit"`
	}{
		Default: dirs{
			Languages: languages, AppDir: "/Applications", AppImageDir: filepath.Join(home, "Applications"),
			KeyboardLayoutDir: "/Library/Keyboard Layouts", ColorPickerDir: lib("ColorPickers"),
			PrefPaneDir: lib("PreferencePanes"), QLPluginDir: lib("QuickLook"), MDImporterDir: lib("Spotlight"),
			DictionaryDir: lib("Dictionaries"), FontDir: lib("Fonts"), ServiceDir: lib("Services"),
			InputMethodDir: lib("Input Methods"), InternetPluginDir: lib("Internet Plug-Ins"),
			AudioUnitPluginDir: lib("Audio/Plug-Ins/Components"), VSTPluginDir: lib("Audio/Plug-Ins/VST"),
			VST3PluginDir: lib("Audio/Plug-Ins/VST3"), ScreenSaverDir: lib("Screen Savers"),
		},
		Explicit: appDir{in.Config.ExplicitAppDir},
	})
	if err != nil {
		return err
	}
	return in.writeMetadata(c, "config.json", b)
}

func (in *Installer) writeReceipt(c api.Cask, p installPlan, onRequest bool) error {
	type source struct {
		Tap        string  `json:"tap"`
		TapGitHead *string `json:"tap_git_head"`
		Version    string  `json:"version"`
		Path       string  `json:"path"`
	}
	var head *string
	if c.TapGitHead != "" {
		head = &c.TapGitHead
	}
	fromAPI, path := c.SourceFile == "", in.APIPath
	if !fromAPI {
		path = c.SourceFile
	}
	arch := "arm64"
	if in.Host.Arch == "amd64" {
		arch = "x86_64"
	}
	b, err := json.MarshalIndent(struct {
		HomebrewVersion       string                       `json:"homebrew_version"`
		LoadedFromAPI         bool                         `json:"loaded_from_api"`
		LoadedFromInternalAPI bool                         `json:"loaded_from_internal_api"`
		UninstallFlightBlocks bool                         `json:"uninstall_flight_blocks"`
		InstalledOnRequest    bool                         `json:"installed_on_request"`
		Time                  int64                        `json:"time"`
		RuntimeDependencies   struct{}                     `json:"runtime_dependencies"`
		Source                source                       `json:"source"`
		Arch                  string                       `json:"arch"`
		UninstallArtifacts    []map[string]json.RawMessage `json:"uninstall_artifacts"`
		BuiltOn               map[string]string            `json:"built_on"`
	}{
		HomebrewVersion: in.Version, LoadedFromAPI: fromAPI, LoadedFromInternalAPI: fromAPI,
		InstalledOnRequest: onRequest, Time: in.Now().Unix(),
		Source: source{Tap: c.Tap, TapGitHead: head, Version: c.Version, Path: path},
		Arch:   arch, UninstallArtifacts: p.uninstall,
		BuiltOn: map[string]string{"os": "Macintosh", "os_version": "macOS " + in.Host.MacVersion},
	}, "", "  ")
	if err != nil {
		return err
	}
	return in.writeMetadata(c, "INSTALL_RECEIPT.json", b)
}
