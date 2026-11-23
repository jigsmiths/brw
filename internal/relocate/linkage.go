package relocate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/machoedit"
)

// Linkage is what fixing a built keg's linkage needs to know.
type Linkage struct {
	Prefix string
	// BuildDirs are where the formula was built, whose paths a keg must not
	// keep. "/private/tmp" also covers "/tmp", which CMake writes instead.
	BuildDirs []string
	// PreserveRpath keeps @rpath dylib IDs, as the formula's preserve_rpath does.
	PreserveRpath bool
	Warn          func(string)
}

var (
	variableRef = regexp.MustCompile(`^@(loader_|executable_|r)path`)
	frameworkRx = regexp.MustCompile(`(?:^|/)(([^/]+)\.framework/(?:Versions/[^/]+/)?([^/]+))$`)
)

// FixLinkage is brew's fix_dynamic_linkage for a keg built from source: dylib
// IDs become opt paths, libraries the build linked by a relative or
// build-directory name are found in the keg, rpaths into other kegs go
// through opt, and build-directory or repeated rpaths are dropped. It returns
// the files it changed, keg-relative, which must be signed again (Codesign).
// Universal and 32 bit files are left alone.
func FixLinkage(root *fsx.Root, keg string, l Linkage) ([]string, error) {
	kegAbs, err := root.Abs(keg)
	if err != nil {
		return nil, err
	}
	fx := &fixer{l: l, kegAbs: kegAbs, opt: filepath.Join(l.Prefix, "opt", path.Base(path.Dir(keg)))}
	// Hard links are one file, so only the first name is fixed.
	seen := map[[2]uint64]bool{}
	var files []string
	err = filepath.WalkDir(kegAbs, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			id := [2]uint64{uint64(st.Dev), st.Ino} //nolint:unconvert // int32 on darwin
			if seen[id] {
				return nil
			}
			seen[id] = true
		}
		files = append(files, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	var modified []string
	for _, p := range files {
		fi, err := os.Lstat(p)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		f, err := machoedit.Parse(data)
		if err != nil || !f.IsDylib() && !f.IsBundle() && !f.IsExecutable() {
			continue
		}
		changed, err := fx.fix(f, p)
		if err != nil {
			return nil, err
		}
		if !changed {
			continue
		}
		rel, _ := filepath.Rel(kegAbs, p)
		modified = append(modified, filepath.ToSlash(rel))
		if err := writeKeeping(root, path.Join(keg, filepath.ToSlash(rel)), keg, fi, f.Bytes()); err != nil {
			return nil, err
		}
	}
	return modified, nil
}

// writeKeeping replaces a file with data, keeping its mode and its other
// names in the keg.
func writeKeeping(root *fsx.Root, file, keg string, fi fs.FileInfo, data []byte) error {
	links, err := hardLinks(root, keg, file, fi)
	if err != nil {
		return err
	}
	if err := root.Remove(file); err != nil {
		return err
	}
	if err := root.WriteFile(file, data, fi.Mode().Perm()); err != nil {
		return err
	}
	return relink(root, map[string][]string{file: links})
}

type fixer struct {
	l           Linkage
	kegAbs, opt string
}

func (x *fixer) fix(f *machoedit.File, file string) (changed bool, err error) {
	if id, ok := f.DylibID(); ok {
		if want := x.dylibID(id, file); want != id {
			if err := f.ChangeID(want); err != nil {
				return false, fmt.Errorf("%s: %w", file, err)
			}
			changed = true
		}
	}
	for _, bad := range f.Dylibs() {
		if variableRef.MatchString(bad) {
			continue
		}
		good := bad
		if !strings.HasPrefix(bad, "/") || x.inBuildDir(bad) {
			good = x.fixedName(f, file, bad)
		}
		if good != bad {
			if err := f.ChangeInstallName(bad, good); err != nil {
				return false, fmt.Errorf("%s: %w", file, err)
			}
			changed = true
		}
	}
	for _, bad := range f.Rpaths() {
		if variableRef.MatchString(bad) {
			continue
		}
		if good := x.optName(bad); good != bad {
			if err := f.ChangeRpath(bad, good); err != nil {
				return false, fmt.Errorf("%s: %w", file, err)
			}
			changed = true
		}
	}
	for _, rp := range f.Rpaths() {
		if variableRef.MatchString(rp) {
			continue
		}
		if x.inBuildDir(rp) || len(slices.DeleteFunc(f.Rpaths(), func(s string) bool { return s != rp })) > 1 {
			if err := f.DeleteRpath(rp); err != nil && !errors.Is(err, machoedit.ErrNotFound) {
				return false, fmt.Errorf("%s: %w", file, err)
			}
			changed = true
		}
	}
	return changed, nil
}

// dylibID is brew's dylib_id_for: the opt path of the file's directory with
// the old ID's base name.
func (x *fixer) dylibID(id, file string) string {
	if strings.HasPrefix(id, "/usr/lib/swift/libswift") || (strings.HasPrefix(id, "@rpath") && x.l.PreserveRpath) {
		return id
	}
	rel, _ := filepath.Rel(x.kegAbs, filepath.Dir(file))
	return filepath.Join(x.opt, rel, path.Base(id))
}

// fixedName is brew's fixed_name, which finds a library the build linked by
// a name that won't work once installed. Absolute names are build-directory
// paths that brew no longer has by now, so only the search can find them.
func (x *fixer) fixedName(f *machoedit.File, file, bad string) string {
	if name, ok := (Locations{Prefix: x.l.Prefix, Cellar: filepath.Join(x.l.Prefix, "Cellar")}).relocatedName(bad); ok {
		return name
	}
	lib := filepath.Join(x.kegAbs, "lib")
	if !filepath.IsAbs(bad) {
		switch {
		case (f.IsDylib() || f.IsBundle()) && exists(filepath.Join(filepath.Dir(file), bad)):
			return "@loader_path/" + bad
		case f.IsExecutable() && exists(filepath.Join(lib, bad)):
			return filepath.Join(lib, bad)
		case f.IsExecutable() && exists(filepath.Join(x.kegAbs, "libexec", "lib", bad)):
			return filepath.Join(x.kegAbs, "libexec", "lib", bad)
		}
	}
	if found := findDylib(lib, bad); found != "" {
		return found
	}
	if x.l.Warn != nil {
		x.l.Warn("Could not fix " + bad + " in " + file)
	}
	return bad
}

// findDylib is brew's find_dylib: the first file under lib ending in the
// library's name, or its framework path.
func findDylib(lib, bad string) string {
	suffix := "/" + path.Base(bad)
	if m := frameworkRx.FindStringSubmatch(bad); m != nil && m[2] == m[3] {
		suffix = "/" + m[1]
	}
	var found string
	_ = filepath.WalkDir(lib, func(p string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, suffix) {
			found = p
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// optName is brew's opt_name_for: a path into another keg goes through opt.
func (x *fixer) optName(name string) string {
	cellar := filepath.Join(x.l.Prefix, "Cellar") + "/"
	if !strings.HasPrefix(name, cellar) || strings.HasPrefix(name, x.kegAbs) {
		return name
	}
	parts := strings.SplitN(strings.TrimPrefix(name, cellar), "/", 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return name
	}
	out := filepath.Join(x.l.Prefix, "opt", parts[0])
	if len(parts) == 3 {
		out += "/" + parts[2]
	}
	return out
}

func (x *fixer) inBuildDir(name string) bool {
	for _, d := range x.l.BuildDirs {
		if d == "/private/tmp" && strings.HasPrefix(name, "/tmp/") {
			return true
		}
		if strings.HasPrefix(name, strings.TrimSuffix(d, "/")+"/") {
			return true
		}
		if real, err := filepath.EvalSymlinks(d); err == nil && strings.HasPrefix(name, real+"/") {
			return true
		}
	}
	return false
}
