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
	"syscall"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/sudo"
)

// Config is what brew records in config.json: the directories from flags (Explicit) and from HOMEBREW_CASK_OPTS (Env).
type Config struct {
	Languages []string
	Explicit  map[DirKey]string
	Env       map[DirKey]string
}

type Installer struct {
	Root  *fsx.Root
	Where Where
	// PreviousWhere is where the version being upgraded was installed, which a flag can change for the new one.
	PreviousWhere Where
	Host          platform.Host
	Fetcher       *Fetcher
	// CacheDir is the download cache that step sandboxes may write to; unlike Fetcher it is set on every installer.
	CacheDir    string
	Log         io.Writer
	Warn        io.Writer
	Now         func() time.Time
	Sleep       func(time.Duration)
	Version     string
	APIPath     string
	Config      Config
	Exec        Exec
	Run         sudo.Runner
	Kill        func(pid int, sig syscall.Signal) error
	Verbose     bool
	Debug       bool
	FindService func(ctx context.Context, label string, privilege sudo.Privilege) (bool, error)
	Adopt       bool
	Force       bool
	Sandbox     poststep.SandboxMode
	// RuntimeDeps lists what the receipt records; nil records none.
	RuntimeDeps func(api.Cask) (RuntimeDependencies, error)
	// SatisfyDependencies installs what c needs; nil leaves it to the caller, and Install then fails if it is missing.
	SatisfyDependencies func(ctx context.Context, c api.Cask) error

	// predecessor and successor are the Moved artifacts of the version an upgrade replaces and of the one it installs.
	predecessor, successor []move
	upgrading              bool
	reopen                 []string
	warnedSandbox          bool

	sandboxWarnings []string

	ancestors      []string
	ancestorsKnown bool
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

func (in *Installer) Install(ctx context.Context, c api.Cask) error {
	return in.install(ctx, c, true)
}

// InstallAsDependency records that another cask needs c, as brew does, so it is not on request.
// Its own dependencies are not satisfied: they are in the list of the cask that asked for it.
func (in *Installer) InstallAsDependency(ctx context.Context, c api.Cask) error {
	return in.install(ctx, c, false)
}

func (in *Installer) install(ctx context.Context, c api.Cask, onRequest bool) (err error) {
	p, links, file, err := in.prepare(ctx, c, nil)
	if err != nil {
		return err
	}
	if onRequest {
		if err := in.satisfyDependencies(ctx, c); err != nil {
			return err
		}
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
	if err := in.writeReceipt(c, p, onRequest); err != nil {
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
	if artifact := p.sudoArtifact(); artifact != "" && !sudo.Available(ctx, in.runner()) {
		return installPlan{}, nil, "", brewError("%s: The %s artifact requires sudo, but HOMEBREW_NO_SUDO is set.", c.Token, artifact)
	}
	in.warnAllowUntrusted(p.pkgs)
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
	if err := in.runSteps(ctx, "preflight_steps", c.Token, c.Version, staged, p.preflight); err != nil {
		return err
	}
	if err := in.runInstallers(ctx, c.Token, staged, p.installers); err != nil {
		return err
	}
	for _, pk := range p.pkgs {
		if err := in.installPkg(ctx, c.Token, staged, pk); err != nil {
			return fmt.Errorf("%s: %w", c.Token, err)
		}
	}
	for _, m := range p.moved {
		if err := in.moveArtifact(ctx, version, m, c.AutoUpdates, undo); err != nil {
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
	if err := in.runSteps(ctx, "postflight_steps", c.Token, c.Version, staged, p.postflight); err != nil {
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

func (in *Installer) prefixRel(target string) (string, error) {
	if !in.inside(in.Where.Prefix, target) {
		return "", fmt.Errorf("binary target %s: outside %s: %w", target, in.Where.Prefix, ErrUnsupported)
	}
	return filepath.Rel(in.Where.Prefix, target)
}

// moveArtifact installs one Moved artifact; unless it adopted an existing target, undo reverts it as brew's rollback does.
func (in *Installer) moveArtifact(ctx context.Context, version string, m move, autoUpdates bool, undo *[]func() error) error {
	adopted, err := in.installMoved(ctx, version, m, autoUpdates)
	if err != nil || adopted {
		return err
	}
	staged := filepath.Join(version, m.source)
	*undo = append(*undo, func() error {
		m := m
		m.source = staged
		return in.uninstallMoved(context.WithoutCancel(ctx), m, false)
	})
	return nil
}

func (in *Installer) withAppDir(f func(*fsx.Root) error) error {
	apps, err := fsx.Open(in.Where.AppDir, nil)
	if err != nil {
		return err
	}
	defer func() { _ = apps.Close() }()
	return f(apps)
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
		err = in.Root.SymlinkAny(source, rel)
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
	if dir, err := filepath.EvalSymlinks(in.Where.AppDir); err == nil && in.inside(dir, path) {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		return in.withAppDir(func(apps *fsx.Root) error { return apps.Chmod(rel, perm) })
	}
	if dir, err := filepath.EvalSymlinks(in.Where.Prefix); err == nil && in.inside(dir, path) {
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		return in.Root.Chmod(rel, perm)
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
	defaults := map[DirKey]string{}
	for _, key := range DirKeys {
		defaults[key] = DefaultDir(key, home)
	}
	languages := in.Config.Languages
	if languages == nil {
		languages = []string{}
	}
	langs, err := json.Marshal(languages)
	if err != nil {
		return err
	}
	dirs := orderedDirs(defaults)
	b := slices.Concat([]byte(`{"default":{"languages":`), langs, []byte(`,`), dirs[1:],
		[]byte(`,"env":`), orderedDirs(in.Config.Env), []byte(`,"explicit":`), orderedDirs(in.Config.Explicit), []byte(`}`))
	return in.writeMetadata(c, "config.json", b)
}

func (in *Installer) writeReceipt(c api.Cask, p installPlan, onRequest bool) error {
	type source struct {
		Tap        string  `json:"tap"`
		TapGitHead *string `json:"tap_git_head"`
		Version    string  `json:"version"`
		Path       string  `json:"path"`
	}
	var deps RuntimeDependencies
	if in.RuntimeDeps != nil {
		var err error
		if deps, err = in.RuntimeDeps(c); err != nil {
			return err
		}
	}
	var head *string
	if c.TapGitHead != "" {
		head = &c.TapGitHead
	}
	uninstall := p.uninstall
	if uninstall == nil {
		uninstall = []map[string]json.RawMessage{}
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
		RuntimeDependencies   RuntimeDependencies          `json:"runtime_dependencies"`
		Source                source                       `json:"source"`
		Arch                  string                       `json:"arch"`
		UninstallArtifacts    []map[string]json.RawMessage `json:"uninstall_artifacts"`
		BuiltOn               map[string]string            `json:"built_on"`
	}{
		HomebrewVersion: in.Version, LoadedFromAPI: fromAPI, LoadedFromInternalAPI: fromAPI,
		InstalledOnRequest: onRequest, Time: in.Now().Unix(), RuntimeDependencies: deps,
		Source: source{Tap: c.Tap, TapGitHead: head, Version: c.Version, Path: path},
		Arch:   arch, UninstallArtifacts: uninstall,
		BuiltOn: map[string]string{"os": "Macintosh", "os_version": "macOS " + in.Host.MacVersion},
	}, "", "  ")
	if err != nil {
		return err
	}
	return in.writeMetadata(c, "INSTALL_RECEIPT.json", b)
}
