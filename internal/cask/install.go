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

// Config is what brew records in a cask's config.json. brw does not read
// HOMEBREW_CASK_OPTS, so the env part stays empty.
type Config struct {
	Languages      []string
	ExplicitAppDir string // --appdir on the command line
}

// Installer installs casks as brew does, into a prefix whose Caskroom brew
// and brw share.
type Installer struct {
	Root    *fsx.Root // the prefix
	Apps    *fsx.Root // the applications folder
	Where   Where
	Host    platform.Host
	Fetcher *Fetcher
	Log     io.Writer
	Warn    io.Writer // brew's warnings
	Now     func() time.Time
	Version string // brw's, for the receipt
	APIPath string // brew's cask listing, which the receipt names as the source
	Config  Config
	Exec    Exec // nil runs commands; uninstall's commands go through it
}

func (in *Installer) logf(format string, args ...any) {
	if in.Log != nil {
		_, _ = fmt.Fprintf(in.Log, format+"\n", args...)
	}
}

// brewMessage is one of brew's errors, which are sentences.
type brewMessage string

func (m brewMessage) Error() string { return string(m) }

func brewError(format string, args ...any) error {
	return brewMessage(fmt.Sprintf(format, args...))
}

// warnf writes one of brew's warnings, which go to stderr.
func (in *Installer) warnf(format string, args ...any) {
	if in.Warn != nil {
		_, _ = fmt.Fprintf(in.Warn, "Warning: "+format+"\n", args...)
	}
}

// Install downloads the cask, stages it in the Caskroom, moves its apps and
// links its binaries. A failure undoes what was done.
func (in *Installer) Install(ctx context.Context, c api.Cask) (err error) {
	if msg := deprecation(c, in.Now()); c.Disabled {
		return brewError("Cask '%s' has been %s", c.Token, msg)
	} else if msg != "" {
		in.warnf("%s has been %s", c.Token, msg)
	}
	if err := checkRequirements(c, in.Host); err != nil {
		return err
	}
	p, err := plan(c, in.Where)
	if err != nil {
		return err
	}
	links, err := in.checkTargets(c, p)
	if err != nil {
		return err
	}
	file, err := in.Fetcher.Fetch(ctx, c)
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
		if err == nil {
			return
		}
		in.logf("==> Purging files for version %s of Cask %s", c.Version, c.Token)
		for i := len(undo) - 1; i >= 0; i-- {
			err = errors.Join(err, undo[i]())
		}
		err = errors.Join(err, in.purge(c))
	}()
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
	for _, m := range p.apps {
		if err := in.moveApp(version, m, &undo); err != nil {
			return fmt.Errorf("%s: %w", c.Token, err)
		}
	}
	for i, m := range p.binaries {
		if !links[i] {
			continue
		}
		if err := in.linkBinary(staged, m, &undo); err != nil {
			return fmt.Errorf("%s: %w", c.Token, err)
		}
	}
	if err := in.writeConfig(c); err != nil {
		return err
	}
	if err := in.writeReceipt(c, p); err != nil {
		return err
	}
	in.logf("%s was successfully installed!", badge()+c.Token)
	return nil
}

// badge is brew's install badge with its two spaces, or nothing.
func badge() string {
	if os.Getenv("HOMEBREW_NO_EMOJI") != "" {
		return ""
	}
	if b := os.Getenv("HOMEBREW_INSTALL_BADGE"); b != "" {
		return b + "  "
	}
	return "🍺  "
}

// checkTargets fails before anything is downloaded when an app or binary
// would land on something already there. It says which binaries to link: one
// whose target a formula has linked is skipped with brew's warning.
func (in *Installer) checkTargets(c api.Cask, p installPlan) ([]bool, error) {
	for _, m := range p.apps {
		if _, err := in.appRel(m.target); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Token, err)
		}
		if _, err := os.Lstat(m.target); err == nil {
			return nil, brewError("%s: It seems there is already an App at '%s'.", c.Token, m.target)
		}
	}
	links := make([]bool, len(p.binaries))
	// Compared with resolved links, so resolved too.
	cellar := filepath.Join(in.Where.Prefix, "Cellar")
	if real, err := filepath.EvalSymlinks(cellar); err == nil {
		cellar = real
	}
	cellar += string(filepath.Separator)
	for i, m := range p.binaries {
		if _, err := in.prefixRel(m.target); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Token, err)
		}
		if filepath.IsAbs(m.source) && !in.inside(in.Where.AppDir, m.source) && !in.inside(in.Where.Prefix, m.source) {
			return nil, fmt.Errorf("%s: binary source %s: outside %s and %s: %w", c.Token, m.source, in.Where.AppDir, in.Where.Prefix, ErrUnsupported)
		}
		fi, err := os.Lstat(m.target)
		if errors.Is(err, fs.ErrNotExist) {
			links[i] = true
			continue
		}
		if err != nil {
			return nil, err
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if real, err := filepath.EvalSymlinks(m.target); err == nil && strings.HasPrefix(real, cellar) {
				formula, _, _ := strings.Cut(strings.TrimPrefix(real, cellar), string(filepath.Separator))
				in.warnf("It seems there is already a Binary at '%s' from formula %s; skipping link.", m.target, formula)
				continue
			}
		}
		return nil, brewError("%s: It seems there is already a Binary at '%s'.", c.Token, m.target)
	}
	return links, nil
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

// moveApp moves the staged app to its target, leaves a link to it in its
// place, as brew does, and sets brew's permissions.
func (in *Installer) moveApp(version string, m move, undo *[]func() error) error {
	staged := filepath.Join(version, m.source)
	if _, err := in.Root.Lstat(staged); err != nil {
		abs, _ := in.Root.Abs(staged)
		return brewError("It seems the App source '%s' is not there.", abs)
	}
	rel, err := in.appRel(m.target)
	if err != nil {
		return err
	}
	in.logf("==> Moving App '%s' to '%s'", filepath.Base(m.source), m.target)
	if err := in.Apps.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := in.Root.MoveTo(in.Apps, staged, rel); err != nil {
		return err
	}
	*undo = append(*undo, func() error { return in.Apps.MoveTo(in.Root, rel, staged) })
	if err := in.Root.SymlinkTo(in.Apps, m.target, staged); err != nil {
		return err
	}
	*undo = append(*undo, func() error { return in.Root.Remove(staged) })
	if err := run(context.Background(), "chmod", "-R", "a+rX,go-w", m.target); err != nil {
		return err
	}
	// brew needs this to succeed in /Applications and lets it fail elsewhere;
	// without sudo, brw lets it fail everywhere.
	if group := caskroomGroup(); group != "" {
		_ = run(context.Background(), "chgrp", "-hR", group, m.target)
	}
	return nil
}

// caskroomGroup is the group brew gives apps: admin when the user is in it,
// else the user's own group.
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

// linkBinary links the binary from the prefix's bin, as brew does: through
// the staged version for a relative source, and makes it executable.
func (in *Installer) linkBinary(staged string, m move, undo *[]func() error) error {
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
	in.logf("==> Linking Binary '%s' to '%s'", filepath.Base(m.source), m.target)
	if err := in.Root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
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
	return in.makeExecutable(real)
}

// makeExecutable is brew's chmod +x, through whichever root holds the file.
func (in *Installer) makeExecutable(path string) error {
	fi, err := os.Stat(path)
	if err != nil || fi.Mode().Perm()&0o111 == 0o111 {
		return err
	}
	perm := fi.Mode().Perm() | 0o111
	// path is resolved, so compare it with the resolved folders.
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

// purge removes what the cask left in the Caskroom, as brew's
// purge_versioned_files does, keeping other versions.
func (in *Installer) purge(c api.Cask) error {
	dir := filepath.Join("Caskroom", c.Token)
	err := errors.Join(
		removeIfThere(in.Root, filepath.Join(dir, c.Version)),
		removeIfThere(in.Root, filepath.Join(dir, ".metadata", c.Version)),
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

// writeMetadata writes a file of .metadata, replacing one an earlier install left.
func (in *Installer) writeMetadata(c api.Cask, name string, data []byte) error {
	path := filepath.Join("Caskroom", c.Token, ".metadata", name)
	if err := removeIfThere(in.Root, path); err != nil {
		return err
	}
	return in.Root.WriteFile(path, data, 0o644)
}

// writeConfig writes config.json as brew does: compact, with the default
// folders in brew's order.
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

// writeReceipt writes INSTALL_RECEIPT.json with brew's keys, in brew's order.
func (in *Installer) writeReceipt(c api.Cask, p installPlan) error {
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
		HomebrewVersion: in.Version, LoadedFromAPI: true, LoadedFromInternalAPI: true,
		InstalledOnRequest: true, Time: in.Now().Unix(),
		Source: source{Tap: c.Tap, TapGitHead: head, Version: c.Version, Path: in.APIPath},
		Arch:   arch, UninstallArtifacts: p.uninstall,
		BuiltOn: map[string]string{"os": "Macintosh", "os_version": "macOS " + in.Host.MacVersion},
	}, "", "  ")
	if err != nil {
		return err
	}
	return in.writeMetadata(c, "INSTALL_RECEIPT.json", b)
}
