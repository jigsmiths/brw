package poststep

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/machoedit"
	"github.com/jigsmiths/brw/internal/sudo"
)

const (
	appManagementProbe = ".homebrew-write-test"
	defaultGroup       = "staff"
	codesignPath       = "codesign"
	chownPath          = "chown"
	touchPath          = "touch"
	rmPath             = "rm"
	userWrite          = 0o200
	groupWrite         = 0o020
	otherWrite         = 0o002
)

var codesignFlags = []string{"--sign", "-", "--force", "--preserve-metadata=entitlements,requirements,flags,runtime"}

func (r *runner) runner() sudo.Runner {
	if r.c.Sudo != nil {
		return r.c.Sudo
	}
	return sudo.Exec
}

func (r *runner) runCommand(exe string, args ...string) error {
	c := sudo.Command{Exe: exe, Args: args, Privilege: sudo.User, MustSucceed: true, Stdout: r.c.Log, Stderr: r.stderr()}
	_, err := r.sudoCommand(c)
	return err
}

func (r *runner) commandOutput(exe string, args ...string) (string, error) {
	c := sudo.Command{Exe: exe, Args: args, Privilege: sudo.User, MustSucceed: true, Stderr: r.stderr()}
	res, err := r.sudoCommand(c)
	return res.Stdout, err
}

// runAsUserThenSudo is brew's `sudo: nil`: the command runs as the user and, only if it fails
// and sudo is usable, once more with sudo. The user's attempt is quiet while sudo may follow.
func (r *runner) runAsUserThenSudo(exe string, args ...string) error {
	asUser := sudo.Command{Exe: exe, Args: args, Privilege: sudo.User, MustSucceed: true}
	sudoMayFollow := !sudo.EnvSet("HOMEBREW_NO_SUDO")
	if sudoMayFollow {
		asUser.Stderr = io.Discard
	}
	_, userErr := r.sudoCommand(asUser)
	if !r.worthRetryingWithSudo(userErr) {
		return userErr
	}
	withSudo := asUser
	withSudo.Privilege, withSudo.Stderr = sudo.Sudo, nil
	_, err := r.sudoCommand(withSudo)
	return err
}

func (r *runner) worthRetryingWithSudo(userErr error) bool {
	return userErr != nil && sudo.IsFailure(userErr) && sudo.Available(r.ctx, r.runner())
}

func (r *runner) keg(name string) string {
	p, _ := r.base(name, "")
	return p
}

func (r *runner) optPrefix(formula string) string {
	return filepath.Join(r.c.Prefix, "opt", nameFromFullName(formula))
}

func nameFromFullName(name string) string {
	if parts := strings.SplitN(name, "/", 3); len(parts) == 3 {
		return parts[2]
	}
	return name
}

func (r *runner) anyVersionInstalled(formula string) bool {
	entries, err := os.ReadDir(filepath.Join(r.c.Prefix, "Cellar", nameFromFullName(formula)))
	if err != nil {
		return false
	}
	for _, e := range entries {
		if fi, err := os.Stat(filepath.Join(r.c.Prefix, "Cellar", nameFromFullName(formula), e.Name(), "INSTALL_RECEIPT.json")); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func (r *runner) formulaTool(formula, exe string, args ...string) error {
	tool := filepath.Join(r.optPrefix(formula), "bin", exe)
	if fi, err := os.Stat(tool); err != nil || fi.IsDir() || syscall.Access(tool, 1) != nil {
		return fmt.Errorf("%s is missing required executable: %s", formula, tool)
	}
	return r.runCommand(tool, args...)
}

func (r *runner) pathTool(s step, formula, exe string) error {
	dir, err := r.target(s.Path)
	if err != nil {
		return err
	}
	return r.formulaTool(formula, exe, dir)
}

func (r *runner) versionMajor() (string, bool) {
	if r.c.Version == "" {
		return "", false
	}
	return strings.SplitN(r.c.Version, ".", 2)[0], true
}

func (r *runner) versionMajorMinor() (string, bool) {
	if r.c.Version == "" {
		return "", false
	}
	parts := strings.SplitN(r.c.Version, ".", 3)
	return strings.Join(parts[:min(2, len(parts))], "."), true
}

func (r *runner) existingPaths(s step) ([]string, error) {
	var paths []string
	for _, spec := range s.Paths {
		matches, err := r.glob(spec)
		if err != nil {
			return nil, err
		}
		for _, p := range matches {
			if _, err := os.Stat(p); err == nil {
				paths = append(paths, p)
			}
		}
	}
	return paths, nil
}

func (r *runner) moveContents(s step) error {
	source, err := r.target(s.Source)
	if err != nil {
		return err
	}
	target, err := r.target(s.Target)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(target); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := filepath.Join(source, e.Name())
		if child == target {
			continue
		}
		if err := r.mvInto(child, target); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) installGzippedExecutable(s step) error {
	source, err := r.target(s.Source)
	if err != nil {
		return err
	}
	if _, err := os.Stat(source); err != nil {
		return nil
	}
	target, err := r.target(s.Target)
	if err != nil {
		return err
	}
	if err := r.mkdirAll(filepath.Dir(target)); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(target), "."+filepath.Base(target)+".install-step")
	if err := r.removeAny(temp); err != nil {
		return err
	}
	defer func() { _ = r.removeAny(temp) }()
	if err := r.gunzip(source, temp); err != nil {
		return err
	}
	if err := r.removeAny(target); err != nil {
		return err
	}
	if err := r.rename(temp, target); err != nil {
		return err
	}
	if err := r.removeAny(source); err != nil {
		return err
	}
	return r.chmod(target, 0o755)
}

func (r *runner) gunzip(source, dest string) error {
	f, err := os.Open(source)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	return r.writeFrom(dest, gz, 0o644)
}

func (r *runner) setOwnership(s step) error {
	paths, err := r.existingPaths(s)
	if err != nil || len(paths) == 0 {
		return err
	}
	for _, p := range paths {
		granted, err := r.appManagementGranted(p)
		if err != nil {
			return err
		}
		if !granted {
			return fmt.Errorf("Cannot change the ownership of '%s' because your terminal does not have App Management permissions.\n"+ //nolint:staticcheck // brew's message, shown to the user
				"macOS prevents modifying apps without these permissions, even when using `sudo`.\n"+
				"To fix this, approve the permissions prompt (if one was just shown) or go to\n"+
				"System Settings → Privacy & Security → App Management and add or enable your terminal.\n"+
				"Then run this command again.\n", p)
		}
	}
	r.logf("==> Changing ownership of paths required by %s...", r.c.Name)
	var args []string
	if !s.NonRecursive {
		args = append(args, "-R")
	}
	owner := s.User
	if owner == "" {
		if owner, err = currentUser(); err != nil {
			return err
		}
	}
	group := s.Group
	if group == "" {
		group = defaultGroup
	}
	args = append(args, "--", owner+":"+group)
	return r.runAsUserThenSudo(chownPath, append(args, paths...)...)
}

func (r *runner) appManagementGranted(app string) (bool, error) {
	if fi, err := os.Stat(app); err != nil || !fi.IsDir() {
		return true, nil
	}
	probe := filepath.Join(app, appManagementProbe)
	fi, err := os.Lstat(app)
	if err != nil {
		return false, err
	}
	if looksWritableWithoutSudo(fi) {
		if err := os.WriteFile(probe, nil, 0o666); err == nil { //fsx:allow brew's App Management probe inside the app
			_ = os.Remove(probe) //fsx:allow brew's App Management probe inside the app
			return true, nil
		} else if !errors.Is(err, fs.ErrPermission) {
			return false, err
		}
	} else {
		err := r.runAsUserThenSudo(touchPath, probe)
		if err == nil {
			return true, r.runAsUserThenSudo(rmPath, probe)
		}
		if !strings.Contains(err.Error(), "touch: "+probe+": Operation not permitted") {
			return false, err
		}
	}
	if sudo.EnvSet("HOMEBREW_NO_APP_MANAGEMENT_PERMISSIONS_PROMPT") {
		r.logf("Warning: Your terminal does not have App Management permissions, so brw will delete and reinstall the app.\n" +
			"This may result in some configurations (like notification settings or location in the Dock/Launchpad) being lost.\n" +
			"To fix this, go to System Settings → Privacy & Security → App Management and add or enable your terminal.")
	}
	return false, nil
}

func looksWritableWithoutSudo(fi fs.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	mode := fi.Mode().Perm()
	switch {
	case int(st.Uid) == os.Geteuid():
		return mode&userWrite != 0
	case inGroup(int(st.Gid)):
		return mode&groupWrite != 0
	}
	return mode&otherWrite != 0
}

func inGroup(gid int) bool {
	if os.Getegid() == gid {
		return true
	}
	groups, _ := os.Getgroups()
	return slices.Contains(groups, gid)
}

func (r *runner) changeDylibID(s step) error {
	file, err := r.target(s.Source)
	if err != nil {
		return err
	}
	if s.ResolveSource {
		if file, err = filepath.EvalSymlinks(file); err != nil {
			return err
		}
	}
	id := r.expand(s.ID)
	return r.ensureWritable(file, func() error {
		data, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		m, err := machoedit.Parse(data)
		if err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		if err := m.ChangeID(id); err != nil {
			return fmt.Errorf("%s: %w", file, err)
		}
		fi, err := os.Stat(file)
		if err != nil {
			return err
		}
		if err := r.overwrite(file, m.Bytes(), fi.Mode().Perm()); err != nil {
			return err
		}
		if r.c.GOARCH == "arm64" {
			if err := r.runCommand(codesignPath, append(slices.Clone(codesignFlags), file)...); err != nil {
				return fmt.Errorf("%s: signing failed!", file) //nolint:staticcheck // ruby-macho's message, shown to the user
			}
		}
		return nil
	})
}

func (r *runner) ensureWritable(file string, do func() error) (err error) {
	if syscall.Access(file, accessWrite) != nil {
		fi, err := os.Stat(file)
		if err != nil {
			return err
		}
		saved := fi.Mode().Perm()
		if err := r.chmod(file, saved|userWrite|0o400); err != nil {
			return err
		}
		defer func() {
			if cerr := r.chmod(file, saved); err == nil {
				err = cerr
			}
		}()
	}
	return do()
}

func (r *runner) macVersion() string {
	major, _, _ := strings.Cut(r.c.MacVersion, ".")
	return major
}

func (r *runner) kernelMajor() (string, error) {
	major, _, _ := strings.Cut(strings.TrimSpace(r.c.Kernel), ".")
	if major == "" {
		return "", errors.New("Clang system configuration requires a kernel version") //nolint:staticcheck // brew's message, shown to the user
	}
	return major, nil
}

const commandLineTools = "/Library/Developer/CommandLineTools"

func (r *runner) configureClangSystem() error {
	if r.c.OS != "darwin" {
		return nil
	}
	mac := r.macVersion()
	kernel, err := r.kernelMajor()
	if err != nil {
		return err
	}
	arch := "x86_64"
	if r.c.GOARCH == "arm64" {
		arch = "arm64"
	}
	dir := filepath.Join(r.keg("etc"), "clang")
	arches := []string{"arm64", "x86_64", "aarch64"}
	if !slices.Contains(arches, arch) {
		arches = append(arches, arch)
	}
	systems := []struct{ name, version string }{{"darwin", kernel}, {"macosx", mac}}
	all := true
	for _, a := range arches {
		for _, sys := range systems {
			if _, err := os.Stat(filepath.Join(dir, a+"-apple-"+sys.name+sys.version+".cfg")); err != nil {
				all = false
			}
		}
	}
	if all {
		return nil
	}
	if err := r.mkdirAll(dir); err != nil {
		return err
	}
	config := "-isysroot " + commandLineTools + "/SDKs/MacOSX" + mac + ".sdk\n"
	for _, sys := range systems {
		for _, a := range arches {
			file := filepath.Join(dir, a+"-apple-"+sys.name+sys.version+".cfg")
			if err := r.removeAny(file); err != nil {
				return err
			}
			if err := r.writeFile(file, []byte(config), 0o644); err != nil {
				return err
			}
		}
	}
	return nil
}

func sudoCommandFor(exe string, args ...string) sudo.Command {
	return sudo.Command{Exe: exe, Args: args, Privilege: sudo.User}
}

func (r *runner) mvTo(source, dest string) error {
	if fi, err := os.Lstat(dest); err == nil && !fi.IsDir() {
		if err := r.removeAny(dest); err != nil {
			return err
		}
	}
	return r.rename(source, dest)
}

func (r *runner) mvInto(source, dir string) error {
	dest := filepath.Join(dir, filepath.Base(source))
	if fi, err := os.Lstat(dest); err == nil && !fi.IsDir() {
		if err := r.removeAny(dest); err != nil {
			return err
		}
	}
	return r.rename(source, dest)
}

func (r *runner) installSymlink(dir, source, name string) error {
	if err := r.mkdirAll(dir); err != nil {
		return err
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(source) {
		source = filepath.Join(real, source)
	}
	if parent, err := filepath.EvalSymlinks(filepath.Dir(source)); err == nil {
		source = filepath.Join(parent, filepath.Base(source))
	}
	text, err := filepath.Rel(real, source)
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, name)
	if err := r.removeAny(dest); err != nil {
		return err
	}
	return r.symlinkAt(text, dest)
}

func (r *runner) atomicWrite(file, content string) error {
	if err := r.mkdirAll(filepath.Dir(file)); err != nil {
		return err
	}
	if err := r.removeAny(file); err != nil {
		return err
	}
	return r.writeFile(file, []byte(content), 0o644)
}
