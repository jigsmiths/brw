package cask

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/sudo"
)

const (
	altNameAttr     = "com.apple.metadata:kMDItemAlternateNames"
	mdimportTool    = "/usr/bin/mdimport"
	qlmanageTool    = "/usr/bin/qlmanage"
	intlDataCache   = "/System/Library/Caches/com.apple.IntlDataCache.le*"
	firstSonomaMac  = 14
	stagedDirMode   = 0o755
	appPermissions  = "go-w"
	systemReadables = "a+rX,"
)

// movedKind is one Cask::Artifact::Moved subclass.
type movedKind struct {
	english string
	// dir is where a relative target goes; hasDir is false for the generic artifact, whose relative targets stay as written.
	dir    DirKey
	hasDir bool
}

var movedKinds = map[string]movedKind{
	"app":               {"App", DirApp, true},
	"suite":             {"App Suite", DirApp, true},
	"artifact":          {"Generic Artifact", "", false},
	"colorpicker":       {"Colorpicker", DirColorPicker, true},
	"prefpane":          {"Preference Pane", DirPrefPane, true},
	"qlplugin":          {"Quick Look Plugin", DirQLPlugin, true},
	"mdimporter":        {"Spotlight metadata importer", DirMDImporter, true},
	"dictionary":        {"Dictionary", DirDictionary, true},
	"font":              {"Font", DirFont, true},
	"service":           {"Service", DirService, true},
	"input_method":      {"Input Method", DirInputMethod, true},
	"internet_plugin":   {"Internet Plugin", DirInternetPlug, true},
	"keyboard_layout":   {"Keyboard Layout", DirKeyboard, true},
	"audio_unit_plugin": {"Audio Unit Plugin", DirAudioUnitPlug, true},
	"vst_plugin":        {"VST Plugin", DirVSTPlug, true},
	"vst3_plugin":       {"VST3 Plugin", DirVST3Plug, true},
	"screen_saver":      {"Screen Saver", DirScreenSaver, true},
}

func isMoved(kind string) bool { _, ok := movedKinds[kind]; return ok }

func (k movedKind) name() string { return k.english }

func (k movedKind) article() string {
	if strings.ContainsRune("aeiouAEIOU", rune(k.english[0])) {
		return "an"
	}
	return "a"
}

func (k movedKind) existing(path string) string {
	return fmt.Sprintf("It seems there is already %s %s at '%s'", k.article(), k.english, path)
}

func occupied(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func present(path string) bool { _, err := os.Stat(path); return err == nil }

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// openDir opens dir for writing through fsx; the caller closes it.
func openDir(dir string) (*fsx.Root, error) { return fsx.Open(dir, nil) }

func (in *Installer) gainPermissionsMkpath(ctx context.Context, dir string) error {
	existing := dir
	for !isDir(existing) {
		parent := filepath.Dir(existing)
		if parent == existing {
			break
		}
		existing = parent
	}
	if existing == dir {
		return nil
	}
	if !writable(existing) {
		return in.sudoIfNeeded(ctx, "mkdir", "-p", "--", dir)
	}
	root, err := openDir(existing)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	rel, err := filepath.Rel(existing, dir)
	if err != nil {
		return err
	}
	return root.MkdirAll(rel, stagedDirMode)
}

// installMoved installs one artifact of any Moved kind; adopted tells the caller not to undo it on failure.
func (in *Installer) installMoved(ctx context.Context, version string, m move, autoUpdates bool) (adopted bool, err error) {
	if adopted, err = in.moveIn(ctx, version, m, autoUpdates); err != nil {
		return adopted, err
	}
	return adopted, in.afterInstall(ctx, m)
}

func (in *Installer) moveIn(ctx context.Context, version string, m move, autoUpdates bool) (adopted bool, err error) {
	k := movedKinds[m.kind]
	staged := filepath.Join(version, m.source)
	abs, err := in.Root.Abs(staged)
	if err != nil {
		return false, err
	}
	if _, err := in.Root.Lstat(staged); err != nil {
		return false, brewError("It seems the %s source '%s' is not there.", k.name(), abs)
	}
	switch {
	case occupied(m.target) && in.Adopt:
		return true, in.adoptIn(ctx, staged, abs, m, autoUpdates)
	case occupied(m.target):
		return false, brewError("%s.", k.existing(m.target))
	}
	in.logf("==> Moving %s '%s' to '%s'", k.name(), filepath.Base(m.source), m.target)
	dir := filepath.Dir(m.target)
	if !present(dir) {
		if err := in.gainPermissionsMkpath(ctx, dir); err != nil {
			return false, err
		}
	}
	switch {
	case writable(dir):
		err = in.moveWhole(ctx, staged, m.target)
	default:
		if err = in.sudoIfNeeded(ctx, "/bin/cp", "-pR", abs, m.target); err == nil {
			err = in.removePath(ctx, in.Root, staged)
		}
	}
	if err != nil {
		return false, err
	}
	return false, in.postMove(ctx, staged, m)
}

func (in *Installer) moveWhole(ctx context.Context, staged, target string) error {
	root, err := openDir(filepath.Dir(target))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	return in.Root.MoveTo(root, staged, filepath.Base(target))
}

func (in *Installer) adoptIn(ctx context.Context, staged, abs string, m move, autoUpdates bool) error {
	k := movedKinds[m.kind]
	in.logf("==> Adopting existing %s at '%s'", k.name(), m.target)
	if !autoUpdates {
		same, err := in.sameApp(ctx, abs, m.target)
		if err != nil {
			return err
		}
		if !same {
			return brewError("It seems the existing %s is different from the one being installed.", k.name())
		}
	}
	if err := in.Root.RemoveAll(staged); err != nil {
		return err
	}
	return in.postMove(ctx, staged, m)
}

func (in *Installer) postMove(ctx context.Context, staged string, m move) error {
	if err := in.Root.SymlinkAny(m.target, staged); err != nil {
		return err
	}
	return in.addAltname(ctx, m.target, filepath.Base(m.source))
}

var altNameList = regexp.MustCompile(`^\((.*)\)\n?$`)

func (in *Installer) addAltname(ctx context.Context, file, altname string) error {
	if in.Host.OS != "darwin" || !in.quarantineAvailable() || strings.EqualFold(altname, filepath.Base(file)) {
		return nil
	}
	out, _ := in.system(ctx, xattrTool, "-p", altNameAttr, file)
	altnames := strings.TrimSuffix(string(out), "\n")
	if m := altNameList.FindStringSubmatch(string(out)); m != nil {
		altnames = m[1]
	}
	if altnames != "" {
		altnames += ", "
	}
	altnames = "(" + altnames + `"` + altname + `")`
	paths := []string{file}
	if real, err := filepath.EvalSymlinks(file); err == nil {
		paths = append(paths, real)
	}
	if err := in.sudoIfNeeded(ctx, "chmod", append([]string{"--", "u+rw"}, paths...)...); err != nil {
		return err
	}
	return in.sudoIfNeeded(ctx, xattrTool, "-w", altNameAttr, altnames, file)
}

func (in *Installer) afterInstall(ctx context.Context, m move) error {
	switch m.kind {
	case "app":
		return in.fixAppPermissions(ctx, m.target)
	case "mdimporter":
		_, err := in.system(ctx, mdimportTool, "-r", m.target)
		return err
	case "qlplugin":
		_, err := in.system(ctx, qlmanageTool, "-r")
		return err
	case "keyboard_layout":
		return in.deleteKeyboardLayoutCache(ctx)
	}
	return nil
}

func (in *Installer) afterUninstall(ctx context.Context, m move) error {
	switch m.kind {
	case "qlplugin":
		_, err := in.system(ctx, qlmanageTool, "-r")
		return err
	case "keyboard_layout":
		return in.deleteKeyboardLayoutCache(ctx)
	}
	return nil
}

func (in *Installer) deleteKeyboardLayoutCache(ctx context.Context) error {
	_, err := in.capture(ctx, in.command(sudo.Root, "/bin/rm", "-f", "--", intlDataCache))
	return err
}

// systemDir is whether any folder from target up to the home folder or root is one macOS owns.
func (in *Installer) systemDir(target string) bool {
	parent := filepath.Dir(target)
	for p := target; ; p = filepath.Dir(p) {
		if p == in.Where.Home || p == "/" && p != parent {
			return false
		}
		if slices.Contains(undeletableSystem, p) {
			return true
		}
		if p == "/" {
			return false
		}
	}
}

func (in *Installer) fixAppPermissions(ctx context.Context, target string) error {
	system := in.systemDir(target)
	permissions := appPermissions
	if system {
		permissions = systemReadables + permissions
	}
	if err := in.sudoIfNeeded(ctx, "chmod", "-R", permissions, target); err != nil {
		return err
	}
	group := caskroomGroup()
	if group == "" {
		return nil
	}
	if system {
		return in.sudoIfNeeded(ctx, "chgrp", "-hR", group, target)
	}
	_, _ = in.system(ctx, "chgrp", "-hR", group, target)
	return nil
}

func (in *Installer) uninstallMoved(ctx context.Context, m move, force bool) error {
	if err := in.moveBack(ctx, m, force); err != nil {
		return err
	}
	return in.afterUninstall(ctx, m)
}

func (in *Installer) moveBack(ctx context.Context, m move, force bool) error {
	k := movedKinds[m.kind]
	staged := m.source
	abs, err := in.Root.Abs(staged)
	if err != nil {
		return err
	}
	if dest, err := in.Root.Readlink(staged); err == nil && joinName(filepath.Dir(abs), dest) == m.target {
		if err := in.Root.Remove(staged); err != nil {
			return err
		}
	}
	if occupied(abs) {
		message := k.existing(abs)
		if !force {
			return brewError("%s.", message)
		}
		in.warnf("%s; overwriting.", message)
		if err := in.deleteMoved(ctx, m, abs); err != nil {
			return err
		}
	}
	if !present(m.target) {
		return nil
	}
	in.logf("==> Backing up %s '%s' to '%s'", k.name(), filepath.Base(m.target), abs)
	if err := in.Root.MkdirAll(filepath.Dir(staged), stagedDirMode); err != nil {
		return err
	}
	if err := in.sudoIfNeeded(ctx, "/bin/cp", in.backupArgs(m.target, abs)...); err != nil {
		return err
	}
	return in.deleteMoved(ctx, m, m.target)
}

func (in *Installer) deleteMoved(ctx context.Context, m move, path string) error {
	k := movedKinds[m.kind]
	in.logf("==> Removing %s '%s'", k.name(), path)
	if undeletable(path, in.Where.Home) {
		return brewError("Cannot remove undeletable %s.", k.name())
	}
	return in.gainPermissionsRemove(ctx, path)
}

func (in *Installer) backupArgs(target, dest string) []string {
	args := []string{"-pR", target, dest}
	if runtime.GOOS != "darwin" {
		return args
	}
	if major, err := versionMajor(in.Host.MacVersion); err != nil || major < firstSonomaMac {
		return args
	}
	a, errA := os.Stat(target)
	b, errB := os.Stat(filepath.Dir(dest))
	if errA != nil || errB != nil {
		return args
	}
	sa, okA := a.Sys().(*syscall.Stat_t)
	sb, okB := b.Sys().(*syscall.Stat_t)
	if !okA || !okB || sa.Dev != sb.Dev {
		return args
	}
	return append([]string{"-c"}, args...)
}
