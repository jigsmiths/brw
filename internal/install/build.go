package install

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/rb"
	"github.com/jigsmiths/brw/internal/sbom"
)

// Source is a tap formula's file, which a build runs.
type Source struct {
	Formula *rb.Formula
	Path    string // the formula file, as the receipt records it
	Code    []byte
}

// ErrNoSource is returned for a formula that has no bottle for this host and
// whose file brw cannot load to build it.
var ErrNoSource = errors.New("cannot be built: no formula file")

// unpack extracts a download into dir as brew's unpack strategies do: tar
// and zip archives are extracted, anything else is copied in under its name.
// Extraction is left to tar and unzip, which write outside fsx as git does.
func (in *Installer) unpack(archive, name, dir string) error {
	lower := strings.ToLower(name)
	var cmd *exec.Cmd
	switch {
	case strings.HasSuffix(lower, ".zip"):
		cmd = exec.Command("/usr/bin/unzip", "-qq", "-o", archive, "-d", dir)
	case strings.Contains(lower, ".tar") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tbz") ||
		strings.HasSuffix(lower, ".txz") || strings.HasSuffix(lower, ".tbz2"):
		cmd = exec.Command("/usr/bin/tar", "-xf", archive, "-C", dir)
	default:
		// copied as brew's Uncompressed strategy does, keeping the download's time
		fi, err := os.Stat(archive)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(archive)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, name)
		if err := in.Root.WriteFile(dst, data, fi.Mode().Perm()); err != nil {
			return err
		}
		return in.Root.Chtimes(dst, fi.ModTime(), fi.ModTime())
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("unpacking %s: %w: %s", name, err, out)
	}
	return nil
}

// buildpath is where brew runs install: in the only directory the download
// unpacked to, if that is all it holds.
func buildpath(dir string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var visible []fs.DirEntry
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			visible = append(visible, e)
		}
	}
	if len(visible) == 0 {
		return "", errors.New("empty archive")
	}
	if len(visible) == 1 && visible[0].IsDir() {
		return filepath.Join(dir, visible[0].Name()), nil
	}
	return dir, nil
}

// newestFile is brew's source_modified_time: the newest mtime under dir.
func newestFile(dir string) time.Time {
	var newest time.Time
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if fi, err := d.Info(); err == nil && fi.ModTime().After(newest) {
			newest = fi.ModTime()
		}
		return nil
	})
	return newest
}

// bottleBlock is what brew strips from the formula file it keeps in the keg.
var bottleBlock = regexp.MustCompile(`(?s)  bottle do.+?end\n\n?`)

// build installs a tap formula from its download, as brew builds a formula
// that has no bottle: stage it, run its install method into the keg, then
// write the receipt, metafiles and SBOM and clean the keg.
func (in *Installer) build(ctx context.Context, it Item, download string) (result Result, err error) {
	f := it.Formula
	if in.LoadSource == nil {
		return Result{}, ErrNoSource
	}
	src, err := in.LoadSource(f)
	if err != nil {
		return Result{}, err
	}
	var old *oldKegs
	if len(it.Upgrade) > 0 {
		if old, err = in.leaveOldKegs(it); err != nil {
			return Result{}, err
		}
		defer func() {
			if err != nil {
				old.restore()
			} else {
				old.commit()
			}
		}()
	}
	in.logf("==> Installing %s from %s", f.Name, f.Tap)

	dest := path.Join(fsx.Staging, randomID())
	defer func() {
		err = errors.Join(err, in.Root.RemoveStaging(dest))
		_ = in.Root.Remove(fsx.Staging) // only succeeds when empty
	}()
	if err := in.Root.MkdirAll(dest, 0o755); err != nil {
		return Result{}, err
	}
	stage, err := in.Root.Abs(dest)
	if err != nil {
		return Result{}, err
	}
	if err := in.unpack(download, path.Base(f.URLs.Stable.URL), stage); err != nil {
		return Result{}, err
	}
	bp, err := buildpath(stage)
	if err != nil {
		return Result{}, err
	}
	modified := newestFile(bp)

	rack := path.Join("Cellar", f.Name)
	keg := path.Join(rack, it.PkgVersion)
	if in.exists(keg) {
		return Result{}, fmt.Errorf("%s is already there", keg)
	}
	madeRack := !in.exists(rack)
	if err := in.Root.MkdirAll(keg, 0o755); err != nil {
		return Result{}, err
	}
	defer func() {
		if err != nil {
			_ = in.Root.RemoveAll(keg)
			if madeRack {
				_ = in.Root.Remove(rack)
			}
		}
	}()
	// As superenv's PATH: the dependencies' programs, then the system's.
	var paths []string
	for _, d := range it.RuntimeDeps {
		paths = append(paths, filepath.Join(in.Prefix, "opt", path.Base(d.FullName), "bin"))
	}
	paths = append(paths, "/usr/bin", "/bin", "/usr/sbin", "/sbin")
	host := &kegHost{root: in.Root, run: in.Run}
	env := &rb.InstallEnv{Host: host, Prefix: in.Prefix, Buildpath: bp, Env: map[string]string{"PATH": strings.Join(paths, ":")}}
	if err := src.Formula.Install(env); err != nil {
		return Result{}, err
	}

	b := cellar.Build{
		HomebrewVersion: in.Version, OnRequest: it.OnRequest, Time: in.now(), SourceModified: modified,
		Aliases: f.Aliases, Deps: it.RuntimeDeps, Tap: f.Tap, TapGitHead: f.TapGitHead, Path: src.Path,
		Stable: f.Versions.Stable, Scheme: f.VersionScheme, Compatibility: f.CompatibilityVersion,
		Arch: it.Arch, BuiltOn: in.BuiltOn,
	}
	receipt, err := cellar.NewBuildReceipt(b)
	if err != nil {
		return Result{}, err
	}
	data, err := receipt.Marshal()
	if err != nil {
		return Result{}, err
	}
	if err := in.Root.WriteFile(path.Join(keg, "INSTALL_RECEIPT.json"), data, 0o644); err != nil {
		return Result{}, err
	}
	if err := in.installMetafiles(bp, keg); err != nil {
		return Result{}, err
	}
	if err := clean(in.Root, keg, src.Formula.SkipClean()); err != nil {
		return Result{}, err
	}
	if err := relativizeSymlinks(in.Root, keg, in.Prefix); err != nil {
		return Result{}, err
	}
	kept := bottleBlock.ReplaceAll(src.Code, nil)
	if err := in.Root.MkdirAll(path.Join(keg, ".brew"), 0o755); err != nil {
		return Result{}, err
	}
	if err := in.Root.WriteFile(path.Join(keg, ".brew", f.Name+".rb"), kept, 0o644); err != nil {
		return Result{}, err
	}
	if err := in.writeBuiltSBOM(keg, f, modified, it.RuntimeDeps); err != nil {
		return Result{}, err
	}
	return in.finish(it, keg, f.Aliases, old), nil
}

// writeBuiltSBOM writes the SBOM brew makes for a formula it builds.
func (in *Installer) writeBuiltSBOM(keg string, f *api.Formula, modified time.Time, deps []cellar.Dependency) error {
	p := sbom.BuildParams{
		Name: f.Name, FullName: f.FullName, Version: f.Versions.Stable, URL: f.URLs.Stable.URL,
		SHA256: f.URLs.Stable.Checksum, License: f.License, Compiler: "clang", Xcode: in.BuiltOn.Xcode,
		SourceModified: modified, Creator: in.SBOMCreator,
	}
	if p.Creator == "" {
		p.Creator = "Tool: https://github.com/jigsmiths/brw"
	}
	for _, d := range deps {
		p.Deps = append(p.Deps, in.sbomDep(d))
	}
	data, err := sbom.Built(p)
	if err != nil {
		return err
	}
	return in.Root.WriteFile(path.Join(keg, sbom.File), data, 0o644)
}

// sbomDep describes a runtime dependency as brew's SBOM.runtime_deps_hash
// does, from the formula it has now.
func (in *Installer) sbomDep(d cellar.Dependency) sbom.Dep {
	out := sbom.Dep{Name: path.Base(d.FullName), FullName: d.FullName, PkgVersion: d.PkgVersion}
	if in.DepInfo != nil {
		out.FormulaPkgVersion, out.License, out.BottleURL, out.BottleSHA256 = in.DepInfo(d.FullName)
	}
	return out
}

var metafileLicenses = map[string]bool{"copying": true, "copyright": true, "license": true, "licence": true}
var metafileExts = map[string]bool{".adoc": true, ".asc": true, ".asciidoc": true, ".creole": true, ".html": true,
	".markdown": true, ".md": true, ".mdown": true, ".mediawiki": true, ".mkdn": true, ".org": true, ".pod": true,
	".rdoc": true, ".rst": true, ".rtf": true, ".textile": true, ".txt": true, ".wiki": true}
var metafileBases = map[string]bool{"about": true, "authors": true, "changelog": true, "changes": true, "history": true,
	"news": true, "notes": true, "notice": true, "readme": true, "todo": true}

// isMetafile is brew's Metafiles.copy?.
func isMetafile(name string) bool {
	file := strings.ToLower(name)
	if first := strings.FieldsFunc(file, func(r rune) bool { return r == '.' || r == '-' }); len(first) > 0 && metafileLicenses[first[0]] {
		return true
	}
	if ext := filepath.Ext(file); metafileExts[ext] {
		file = strings.TrimSuffix(file, ext)
	}
	return metafileBases[file]
}

// installMetafiles is Pathname#install_metafiles: the licence, readme and the
// like at the top of the build directory go into the keg, made 0644.
func (in *Installer) installMetafiles(buildpath, keg string) error {
	entries, err := os.ReadDir(buildpath)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !isMetafile(e.Name()) {
			continue
		}
		src := filepath.Join(buildpath, e.Name())
		fi, err := os.Stat(src)
		if err != nil || fi.IsDir() || fi.Size() == 0 {
			continue
		}
		dst := path.Join(keg, e.Name())
		if in.exists(dst) {
			continue
		}
		if err := in.Root.Rename(src, dst); err != nil {
			return err
		}
		if err := in.Root.Chmod(dst, 0o644); err != nil {
			return err
		}
	}
	return nil
}
