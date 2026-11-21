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
	Sleep   func(time.Duration) // brew's pauses; nil is time.Sleep
	Version string              // brw's, for the receipt
	APIPath string              // brew's cask listing, which the receipt names as the source
	Config  Config
	Exec    Exec // nil runs commands; uninstall's commands go through it
	Adopt   bool // take over an app already at the target, as brew's --adopt

	upgrading     bool     // quit records what it quit, to reopen
	reopen        []string // bundle IDs quit during this upgrade
	warnedSandbox bool
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

// prepare checks that the cask can be installed here and downloads it. owned
// are the targets of the version being upgraded, which it will clear first.
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

// installArtifacts stages the download in the Caskroom, moves the apps, links
// the binaries and writes the config, adding to undo what reverses each step.
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
	for i, m := range p.binaries {
		if links[i] == skipLink {
			continue
		}
		action, _, err := in.binaryAction(c, m)
		if err != nil {
			return err
		}
		switch action {
		case alreadyLinked:
			in.logf("==> Binary '%s' is already linked to '%s'", filepath.Base(m.source), m.target)
			continue
		case skipLink: // a formula linked it since checkTargets
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

// discard reverses a failed install of c and removes its files, as brew's
// purge_versioned_files.
func (in *Installer) discard(ctx context.Context, c api.Cask, undo []func() error) error {
	in.logf("==> Purging files for version %s of Cask %s", c.Version, c.Token)
	var err error
	for _, u := range slices.Backward(undo) {
		err = errors.Join(err, u())
	}
	return errors.Join(err, in.purge(context.WithoutCancel(ctx), c))
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

// linkAction is what linking does to a binary's target, as brew's link_action.
type linkAction int

const (
	newLink       linkAction = iota // nothing there, or a broken link
	overwrite                       // adopting a link of this cask's
	alreadyLinked                   // a link to the source
	skipLink                        // a formula's link
)

// checkTargets fails before any download when a target is taken by something
// other than the replaced version. A binary whose target a formula links is
// skipped with brew's warning.
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
	links := make([]linkAction, len(p.binaries))
	for i, m := range p.binaries {
		if _, err := in.prefixRel(m.target); err != nil {
			return nil, fmt.Errorf("%s: %w", c.Token, err)
		}
		if filepath.IsAbs(m.source) && !in.inside(in.Where.AppDir, m.source) && !in.inside(in.Where.Prefix, m.source) {
			return nil, fmt.Errorf("%s: binary source %s: outside %s and %s: %w", c.Token, m.source, in.Where.AppDir, in.Where.Prefix, ErrUnsupported)
		}
		if owned[m.target] {
			continue // newLink: linking looks again once the old link is gone
		}
		action, formula, err := in.binaryAction(c, m)
		if err != nil {
			return nil, err
		}
		if action == skipLink {
			in.warnf("It seems there is already a Binary at '%s' from formula %s; skipping link.", m.target, formula)
		}
		links[i] = action
	}
	return links, nil
}

// binaryAction is brew's link_action for a binary's target, and the formula
// whose link it is when skipped. It changes once the cask is staged, since
// links into the Caskroom resolve again.
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
		if err != nil { // brew's ln --force replaces a broken link
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
	return 0, "", brewError("%s: It seems there is already a Binary at '%s'.", c.Token, m.target)
}

// linksToSource is brew's target_links_to_source?, for the link at the
// binary's target resolved to real.
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

// formulaLink names the formula whose keg the link at path resolves into.
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

// resolved is dir with its links resolved, to compare with resolved paths;
// dir itself when it cannot be resolved.
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

// moveApp moves the staged app to its target, leaves a link to it in its
// place, as brew does, and sets brew's permissions. When adopting, an app
// already at the target is kept, and kept on rollback too, unlike brew.
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
		// brew copies into a folder only sudo can write, then drops the staged app.
		abs, err := in.Root.Abs(staged)
		if err != nil {
			return err
		}
		if err := in.sudoIfNeeded(ctx, "/bin/cp", "-pR", abs, m.target); err != nil {
			return err
		}
		// Undo runs after a failure, which may be the context's cancellation.
		*undo = append(*undo, func() error { return in.removePath(context.WithoutCancel(ctx), in.Apps, rel) })
		if err := in.removePath(ctx, in.Root, staged); err != nil {
			return err
		}
	}
	return in.linkApp(ctx, staged, m, undo)
}

// linkApp is brew's post_move and app permissions: a link to the app in
// the Caskroom, and the app readable by all.
func (in *Installer) linkApp(ctx context.Context, staged string, m move, undo *[]func() error) error {
	if err := in.Root.SymlinkTo(in.Apps, m.target, staged); err != nil {
		return err
	}
	*undo = append(*undo, func() error { return in.Root.Remove(staged) })
	if err := run(ctx, "chmod", "-R", "a+rX,go-w", m.target); err != nil {
		return err
	}
	// brew needs this to succeed in /Applications and lets it fail elsewhere;
	// without sudo, brw lets it fail everywhere.
	if group := caskroomGroup(); group != "" {
		_ = run(ctx, "chgrp", "-hR", group, m.target)
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
		in.warnf("It seems there is already a Binary at '%s'; overwriting.", m.target)
		if err := in.removePath(ctx, in.Root, rel); err != nil {
			return err
		}
	}
	in.logf("==> Linking Binary '%s' to '%s'", filepath.Base(m.source), m.target)
	if err := in.Root.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
		return err
	}
	if err := removeIfThere(in.Root, rel); err != nil { // a broken link, as ln --force
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
