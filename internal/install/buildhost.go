package install

import (
	"bytes"
	"debug/macho"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/fsx"
)

// RunFunc runs a command for a build: argv in dir, with env added to the
// environment, returning what it prints.
type RunFunc func(dir string, env map[string]string, argv []string) ([]byte, error)

// passedVars are what bin/brew keeps of the user's environment, besides
// HOMEBREW_*; everything else is gone before a formula runs.
var passedVars = []string{"BAT_CONFIG_PATH", "BAT_THEME", "HOME", "SHELL", "PATH", "TERM", "TERMINFO", "TERMINFO_DIRS",
	"COLUMNS", "DISPLAY", "LOGNAME", "USER", "CI", "SSH_AUTH_SOCK", "SUDO_ASKPASS", "http_proxy", "https_proxy",
	"ftp_proxy", "no_proxy", "all_proxy", "HTTPS_PROXY", "FTP_PROXY", "ALL_PROXY"}

// buildEnv is the environment of a build's commands: the user's, filtered as
// bin/brew does, with env set over it.
func buildEnv(env map[string]string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if v == "" || (!strings.HasPrefix(k, "HOMEBREW_") && !slices.Contains(passedVars, k)) {
			continue
		}
		if _, set := env[k]; !set {
			out = append(out, kv)
		}
	}
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	slices.Sort(out)
	return out
}

// runCommand is how a build runs commands unless the installer says otherwise.
func runCommand(dir string, env map[string]string, argv []string) ([]byte, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = buildEnv(env)
	return cmd.Output() // a failure's stderr is in its *exec.ExitError
}

// kegHost is an install body's view of the machine: files only inside the
// prefix, through fsx, and commands.
type kegHost struct {
	root *fsx.Root
	run  RunFunc
}

func (h *kegHost) Lstat(p string) (fs.FileInfo, error)     { return h.root.Lstat(p) }
func (h *kegHost) ReadDir(p string) ([]fs.DirEntry, error) { return h.root.ReadDir(p) }
func (h *kegHost) ReadFile(p string) ([]byte, error)       { return h.root.ReadFile(p) }
func (h *kegHost) MkdirAll(p string) error                 { return h.root.MkdirAll(p, 0o755) }
func (h *kegHost) Rename(o, n string) error                { return h.root.Rename(o, n) }
func (h *kegHost) Symlink(target, link string) error       { return h.root.Symlink(target, link) }
func (h *kegHost) Remove(p string) error                   { return h.root.Remove(p) }
func (h *kegHost) RemoveAll(p string) error                { return h.root.RemoveAll(p) }
func (h *kegHost) Chmod(p string, m fs.FileMode) error     { return h.root.Chmod(p, m) }

func (h *kegHost) WriteFile(p string, data []byte, perm fs.FileMode) error {
	return h.root.WriteFile(p, data, perm)
}

// Glob matches only inside the prefix.
func (h *kegHost) Glob(pattern string) ([]string, error) {
	if _, err := h.root.Abs(pattern); err != nil {
		return nil, err
	}
	return filepath.Glob(pattern)
}

func (h *kegHost) Run(dir string, env map[string]string, argv []string) ([]byte, error) {
	run := h.run
	if run == nil {
		run = runCommand
	}
	return run(dir, env, argv)
}

// executable is Cleaner#executable_path? on macOS: a script with a shebang,
// or a Mach-O executable, which a dylib is not.
func executable(root *fsx.Root, name string) bool {
	data, err := root.ReadFile(name)
	if err != nil {
		return false
	}
	if bytes.HasPrefix(data, []byte("#!")) {
		return true
	}
	r := bytes.NewReader(data)
	if f, err := macho.NewFile(r); err == nil {
		return f.Type == macho.TypeExec
	}
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		return false
	}
	if fat, err := macho.NewFatFile(r); err == nil && len(fat.Arches) > 0 {
		return fat.Arches[0].Type == macho.TypeExec
	}
	return false
}

// skipped is Formula#skip_clean?: a skip_clean path, or what is under it,
// relative to the keg, and .la files when :la is listed.
func skipped(keg, name string, skip []string) bool {
	rel := strings.TrimPrefix(strings.TrimPrefix(name, keg), "/")
	if path.Ext(name) == ".la" && slices.Contains(skip, ":la") {
		return true
	}
	return slices.ContainsFunc(skip, func(s string) bool { return rel == s || strings.HasPrefix(rel, s+"/") })
}

// clean is brew's Cleaner: in bin, sbin and lib, libtool and perl leftovers
// go, and files become 0555 when executable and 0444 otherwise; info's dir
// files go; then empty directories and broken symlinks are pruned. Paths the
// formula's skip_clean names are left alone.
func clean(root *fsx.Root, keg string, skip []string) error {
	_ = root.Remove(path.Join(keg, "lib", "charset.alias"))
	for _, dir := range []string{"bin", "sbin", "lib"} {
		top := path.Join(keg, dir)
		if _, err := root.Lstat(top); err != nil {
			continue
		}
		var files []string
		if err := root.WalkDir(top, func(name string, e fs.DirEntry) {
			if e.Type().IsRegular() && !skipped(keg, name, skip) {
				files = append(files, name)
			}
		}); err != nil {
			return err
		}
		for _, name := range files {
			base := path.Base(name)
			if ext := path.Ext(base); ext == ".la" || ext == ".tbd" || base == "perllocal.pod" || base == ".packlist" {
				if err := root.Remove(name); err != nil {
					return err
				}
				continue
			}
			perm := fs.FileMode(0o444)
			if executable(root, name) {
				perm = 0o555
			}
			if err := root.Chmod(name, perm); err != nil {
				return err
			}
		}
	}
	info := path.Join(keg, "share", "info")
	if _, err := root.Lstat(info); err == nil {
		var dirFiles []string
		_ = root.WalkDir(info, func(name string, e fs.DirEntry) {
			if e.Type().IsRegular() && path.Base(name) == "dir" && !skipped(keg, name, skip) {
				dirFiles = append(dirFiles, name)
			}
		})
		for _, name := range dirFiles {
			if err := root.Remove(name); err != nil {
				return err
			}
		}
	}
	return prune(root, keg, skip)
}

// prune is Cleaner#prune: empty directories, deepest first, and symlinks
// that point nowhere, leaving libexec and skip_clean paths alone.
func prune(root *fsx.Root, keg string, skip []string) error {
	var dirs, links []string
	libexec := path.Join(keg, "libexec")
	if err := root.WalkDir(keg, func(name string, e fs.DirEntry) {
		switch {
		case name == libexec || strings.HasPrefix(name, libexec+"/") || skipped(keg, name, skip):
		case e.Type()&fs.ModeSymlink != 0:
			links = append(links, name)
		case e.IsDir() && name != keg:
			dirs = append(dirs, name)
		}
	}); err != nil {
		return err
	}
	for _, d := range slices.Backward(dirs) {
		if entries, err := root.ReadDir(d); err == nil && len(entries) == 0 {
			if err := root.Remove(d); err != nil {
				return err
			}
		}
	}
	for _, l := range slices.Backward(links) {
		abs, err := root.Abs(l)
		if err != nil {
			continue
		}
		if _, err := os.Stat(abs); err != nil { // follows the link, wherever it points
			_ = root.Remove(l)
		}
	}
	return nil
}

// relativizeSymlinks is Keg#relativize_prefix_symlinks!: an absolute symlink
// into the prefix becomes relative, as relocation never rewrites them.
func relativizeSymlinks(root *fsx.Root, keg, prefix string) error {
	var links []string
	if err := root.WalkDir(keg, func(name string, e fs.DirEntry) {
		if e.Type()&fs.ModeSymlink != 0 {
			links = append(links, name)
		}
	}); err != nil {
		return err
	}
	for _, l := range links {
		target, err := root.Readlink(l)
		if err != nil || !filepath.IsAbs(target) || !strings.HasPrefix(target, prefix+"/") {
			continue
		}
		dir, err := root.Abs(path.Dir(l))
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, target)
		if err != nil {
			return err
		}
		if err := root.Remove(l); err != nil {
			return err
		}
		if err := root.Symlink(rel, l); err != nil {
			return err
		}
	}
	return nil
}
