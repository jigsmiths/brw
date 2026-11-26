package install

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/platform"
	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/rb"
	"github.com/jigsmiths/brw/internal/relocate"
	"github.com/jigsmiths/brw/internal/sbom"
)

type Source struct {
	Formula *rb.Formula
	Path    string // as the receipt records it
	Code    []byte
}

var ErrNoSource = errors.New("cannot be built: no formula file")

// unpack: tar and zip are extracted by tar and unzip, which write outside fsx.
func (in *Installer) unpack(root *fsx.Root, archive, name, dir string) error {
	lower := strings.ToLower(name)
	var cmd *exec.Cmd
	switch {
	case strings.HasSuffix(lower, ".zip"):
		cmd = exec.Command("/usr/bin/unzip", "-qq", "-o", archive, "-d", dir)
	case strings.Contains(lower, ".tar") || strings.HasSuffix(lower, ".tgz") || strings.HasSuffix(lower, ".tbz") ||
		strings.HasSuffix(lower, ".txz") || strings.HasSuffix(lower, ".tbz2"):
		cmd = exec.Command("/usr/bin/tar", "-xf", archive, "-C", dir)
	default:
		// brew's Uncompressed strategy keeps the download's mtime
		fi, err := os.Stat(archive)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(archive)
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, name)
		if err := root.WriteFile(dst, data, fi.Mode().Perm()); err != nil {
			return err
		}
		return root.Chtimes(dst, fi.ModTime(), fi.ModTime())
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

// PostInstallAction is a tap formula's post_install, run in the sandboxed child.
const PostInstallAction = "formula_post_install"

type postInstallRequest struct {
	Name  string `json:"name"`
	Tap   string `json:"tap"`
	Cache string `json:"cache"`
	Code  []byte `json:"code"`
}

// ChildActions is what the sandboxed child can do besides run steps.
func ChildActions() poststep.Actions {
	return poststep.Actions{PostInstallAction: runPostInstallInChild}
}

func runPostInstallInChild(_ context.Context, c poststep.Context, extra json.RawMessage, stdout, _ io.Writer) error {
	var req postInstallRequest
	if err := json.Unmarshal(extra, &req); err != nil {
		return err
	}
	tag, err := platform.Tag(platform.Host{OS: c.OS, MacVersion: c.MacVersion, Arch: c.GOARCH})
	if err != nil {
		return err
	}
	formula, err := rb.LoadFormula(req.Code, req.Name, tag)
	if err != nil {
		return err
	}
	home, temp := os.Getenv("HOME"), os.Getenv("HOMEBREW_TEMP")
	env := sandboxEnv(home, req.Cache, temp)
	env["PATH"] = os.Getenv("PATH")
	return formula.PostInstall(&rb.InstallEnv{
		Host: &kegHost{root: c.Root}, Prefix: c.Prefix, Buildpath: home, Env: env, Tap: req.Tap, Out: stdout,
	})
}

// tapPostInstall is run_post_install: the user's PATH and a temporary home, in the sandbox as brew's postinstall.rb is.
func (in *Installer) tapPostInstall(f *api.Formula, pctx poststep.Context) error {
	if in.LoadSource == nil {
		return ErrNoSource
	}
	src, err := in.LoadSource(f)
	if err != nil {
		return err
	}
	inProcess := func() error {
		temp := cmp.Or(os.Getenv("HOMEBREW_TEMP"), "/private/tmp")
		home, err := os.MkdirTemp(temp, f.Name+"-postinstall-") //fsx:allow a temporary home outside the prefix
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(home) }() //fsx:allow the temporary home
		env := sandboxEnv(home, filepath.Dir(in.Fetcher.Dir), temp)
		env["PATH"] = os.Getenv("PATH")
		return src.Formula.PostInstall(&rb.InstallEnv{
			Host: &kegHost{root: in.Root, run: in.Run}, Prefix: in.Prefix, Buildpath: home, Env: env, Tap: f.Tap, Out: in.Log,
		})
	}
	extra, err := json.Marshal(postInstallRequest{Name: f.Name, Tap: f.Tap, Cache: filepath.Dir(in.Fetcher.Dir), Code: src.Code})
	if err != nil {
		return err
	}
	return poststep.RunAction(context.Background(), pctx, PostInstallAction, extra, inProcess)
}

// applyPatches is Patch.apply; an external patch must be a single patch file.
func (in *Installer) applyPatches(ctx context.Context, name string, patches []rb.Patch, dir string) error {
	for _, p := range patches {
		text := p.Data
		if p.URL != "" {
			lower := strings.ToLower(path.Base(p.URL))
			if strings.Contains(lower, ".tar") || strings.HasSuffix(lower, ".zip") || strings.HasSuffix(lower, ".tgz") {
				return fmt.Errorf("patch %s: patches from archives aren't supported", p.URL)
			}
			file, err := in.Fetcher.FetchSource(ctx, name+"--patch", p.SHA256[:min(len(p.SHA256), 12)], p.URL, p.SHA256)
			if err != nil {
				return fmt.Errorf("patch %s: %w", p.URL, err)
			}
			b, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			text = string(b)
		} else {
			text = strings.ReplaceAll(text, "@@HOMEBREW_PREFIX@@", in.Prefix)
		}
		cmd := exec.CommandContext(ctx, "/usr/bin/patch", "-g", "0", "-f", "-"+p.Strip)
		cmd.Dir, cmd.Stdin = dir, strings.NewReader(text)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("applying a patch: %w\n%s", err, out)
		}
	}
	return nil
}

// removeBuildDir makes read-only directories writable first, as brew's rm_rf.
func removeBuildDir(dir string) error {
	if os.RemoveAll(dir) == nil { //fsx:allow the build directory, outside the prefix
		return nil
	}
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755) //fsx:allow the build directory, outside the prefix
		}
		return nil
	})
	return os.RemoveAll(dir) //fsx:allow the build directory, outside the prefix
}

func makeJobs() int {
	if n, err := strconv.Atoi(os.Getenv("HOMEBREW_MAKE_JOBS")); err == nil && n > 0 {
		return n
	}
	return runtime.NumCPU()
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

func (in *Installer) build(ctx context.Context, it Item, fetched buildSource) (result Result, err error) {
	f := it.Formula
	if in.LoadSource == nil {
		return Result{}, ErrNoSource
	}
	src, err := in.LoadSource(f)
	if err != nil {
		return Result{}, err
	}
	srcSHA, headVersion, version := f.URLs.Stable.Checksum, f.Versions.Head, f.Versions.Stable
	if it.Head {
		src.Formula.UseHead(fetched.headVersion())
		srcSHA, headVersion, version = "", fetched.headVersion(), fetched.headVersion()
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

	// HOMEBREW_TEMP, outside the prefix, which may be a git repository.
	temp := cmp.Or(os.Getenv("HOMEBREW_TEMP"), "/private/tmp")
	top, err := os.MkdirTemp(temp, f.Name+"-"+it.PkgVersion+"-") //fsx:allow the build directory, outside the prefix
	if err != nil {
		return Result{}, err
	}
	defer func() { err = errors.Join(err, removeBuildDir(top)) }()
	if top, err = filepath.EvalSymlinks(top); err != nil {
		return Result{}, err
	}
	broot, err := fsx.Open(top, nil)
	if err != nil {
		return Result{}, err
	}
	defer func() { _ = broot.Close() }()
	for _, d := range []string{"src", "resources"} {
		if err := broot.MkdirAll(d, 0o755); err != nil {
			return Result{}, err
		}
	}
	stage, scratch := filepath.Join(top, "src"), filepath.Join(top, "resources")
	var checkout gitCheckout
	switch s := fetched.(type) {
	case gitCheckout:
		checkout, err = s, copyCheckout(s.dir, stage)
	case archive:
		err = in.unpack(broot, s.path, path.Base(s.url), stage)
	}
	if err != nil {
		return Result{}, err
	}
	bp, err := buildpath(stage)
	if err != nil {
		return Result{}, err
	}
	modified, commit := checkout.time, checkout.revision
	if a, ok := fetched.(archive); ok {
		modified, commit = newestFile(bp), tarCommit(a.path)
	}
	resources, files := map[string]string{}, map[string]string{}
	for _, r := range src.Formula.Resources() {
		if resources[r.Name], err = in.Fetcher.FetchSource(ctx, f.Name+"--"+r.Name, it.PkgVersion, r.URL, r.SHA256); err != nil {
			return Result{}, fmt.Errorf("resource %s: %w", r.Name, err)
		}
		files[resources[r.Name]] = path.Base(r.URL)
	}
	if err := in.applyPatches(ctx, f.Name, src.Formula.Patches(), bp); err != nil {
		return Result{}, err
	}

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
	var bins []string
	for _, d := range it.RuntimeDeps {
		bins = append(bins, filepath.Join(in.Prefix, "opt", path.Base(d.FullName), "bin"))
	}
	for _, d := range it.BuildDeps {
		bins = append(bins, filepath.Join(in.Prefix, "opt", path.Base(d), "bin"))
	}
	envVars := stdenv(stdenvParams{
		Prefix: in.Prefix, Buildpath: bp, Cache: filepath.Dir(in.Fetcher.Dir), SDK: in.SDK, MacVersion: in.MacVersion,
		Temp: temp, Jobs: makeJobs(), DepBins: bins, Keg: filepath.Join(in.Prefix, keg), SourceModified: modified,
	})
	if err := broot.MkdirAll(filepath.Join(bp, ".brew_home"), 0o755); err != nil {
		return Result{}, err
	}
	host := &kegHost{root: in.Root, build: broot, run: in.Run}
	env := &rb.InstallEnv{Host: host, Prefix: in.Prefix, Buildpath: bp, Env: envVars, Tap: f.Tap,
		Resources: resources, Scratch: scratch, TarCommit: commit,
		Unpack: func(archive, dir string) error { return in.unpack(broot, archive, files[archive], dir) }}
	if err := src.Formula.Install(env); err != nil {
		return Result{}, err
	}

	b := cellar.Build{
		HomebrewVersion: in.Version, OnRequest: it.OnRequest, Time: in.now(), SourceModified: modified,
		Aliases: f.Aliases, Deps: it.RuntimeDeps, Tap: f.Tap, TapGitHead: f.TapGitHead, Path: src.Path,
		Stable: f.Versions.Stable, Head: headVersion, Scheme: f.VersionScheme, Compatibility: f.CompatibilityVersion,
		Arch: it.Arch, BuiltOn: in.BuiltOn,
	}
	if it.Head {
		b.HeadBuild, b.SCMRevision = true, checkout.revision
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
	if err := in.installMetafiles(broot, bp, keg); err != nil {
		return Result{}, err
	}
	if err := clean(in.Root, keg, src.Formula.SkipClean()); err != nil {
		return Result{}, err
	}
	if err := relativizeSymlinks(in.Root, keg, in.Prefix); err != nil {
		return Result{}, err
	}
	if runtime.GOOS == "darwin" {
		fixed, err := relocate.FixLinkage(in.Root, keg, relocate.Linkage{
			Prefix: in.Prefix, BuildDirs: []string{envVars["TMPDIR"], top}, PreserveRpath: src.Formula.PreserveRpath(),
			Warn: func(s string) { in.logf("Warning: %s", s) },
		})
		if err != nil {
			return Result{}, err
		}
		if err := relocate.Codesign(in.Root, keg, fixed); err != nil {
			return Result{}, err
		}
	}
	// A bottle has these already; brew's install_service writes them for a build.
	services, err := src.Formula.ServiceFiles(in.Prefix)
	if err != nil {
		return Result{}, err
	}
	for name, body := range services {
		if err := in.Root.WriteFile(path.Join(keg, name), []byte(body), 0o644); err != nil {
			return Result{}, err
		}
		if strings.Contains(body, filepath.Join(in.Prefix, "var", "log")) {
			if err := in.Root.MkdirAll(path.Join("var", "log"), 0o755); err != nil {
				return Result{}, err
			}
		}
	}
	kept := bottleBlock.ReplaceAll(src.Code, nil)
	if err := in.Root.MkdirAll(path.Join(keg, ".brew"), 0o755); err != nil {
		return Result{}, err
	}
	if err := in.Root.WriteFile(path.Join(keg, ".brew", f.Name+".rb"), kept, 0o644); err != nil {
		return Result{}, err
	}
	if err := in.writeBuiltSBOM(keg, f, version, fetched.sourceURL(), srcSHA, modified, it.RuntimeDeps); err != nil {
		return Result{}, err
	}
	return in.finish(it, keg, f.Aliases, old), nil
}

func (in *Installer) writeBuiltSBOM(keg string, f *api.Formula, version, url, sha string, modified time.Time, deps []cellar.Dependency) error {
	p := sbom.BuildParams{
		Name: f.Name, FullName: f.FullName, Version: version, URL: url,
		SHA256: sha, License: f.License, Compiler: "clang", Xcode: in.BuiltOn.Xcode,
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

// sbomDep is SBOM.runtime_deps_hash.
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

// installMetafiles is Pathname#install_metafiles.
func (in *Installer) installMetafiles(build *fsx.Root, buildpath, keg string) error {
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
		if err := build.MoveTo(in.Root, src, dst); err != nil {
			return err
		}
		if err := in.Root.Chmod(dst, 0o644); err != nil {
			return err
		}
	}
	return nil
}
