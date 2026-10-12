package install

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"runtime"
	"slices"
	"time"

	"github.com/jigsmiths/brw/internal/bottle"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/relocate"
)

// Installer carries out a Plan in the prefix that Root is open on.
type Installer struct {
	Root    *fsx.Root
	Prefix  string
	Fetcher *bottle.Fetcher
	Linker  *link.Linker
	// Sign signs edited Mach-O files again. Nil means relocate.Codesign on macOS
	// and nothing elsewhere.
	Sign    func(root *fsx.Root, keg string, files []string) error
	Now     func() time.Time
	APIPath string // recorded in each receipt as the API data the formula came from
	Log     io.Writer
	OS      string // for the post_install guards; runtime.GOOS if empty
	User    string // for {{user}} in post_install steps
}

// standardDirs are the directories every prefix has, which linking relies on.
var standardDirs = []string{"Cellar", "bin", "etc", "include", "lib", "sbin", "share", "opt", "var/homebrew/linked"}

// Result is what happened to one formula.
type Result struct {
	Name, Version string
	Size          string // such as "20 files, 1.3MB"
	Linked        bool
	LinkErr       error // why it is not linked, which does not fail the install
	// PostInstallErr is why the post_install steps failed. The keg stays, as in brew.
	PostInstallErr error
}

func (in *Installer) logf(format string, args ...any) {
	if in.Log != nil {
		_, _ = fmt.Fprintf(in.Log, format+"\n", args...)
	}
}

// Install pours each item in order. It locks every formula first and fetches
// all the bottles in parallel. If a formula fails, those before it stay
// installed and the ones after it are not touched. A formula that cannot be
// linked stays poured and is reported in its Result.
func (in *Installer) Install(ctx context.Context, plan *Plan) (results []Result, err error) {
	if len(plan.Items) == 0 {
		return nil, nil
	}
	unlock, err := in.lockAll(plan.Items)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, unlock()) }()
	dirs := standardDirs
	if cmp.Or(in.OS, runtime.GOOS) == "darwin" {
		dirs = append(slices.Clone(dirs), "Frameworks")
	}
	for _, d := range dirs {
		if err := in.Root.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}

	reqs := make([]bottle.Request, len(plan.Items))
	for i, it := range plan.Items {
		reqs[i] = bottle.Request{Name: it.Formula.Name, PkgVersion: it.PkgVersion, Rebuild: it.Formula.Bottle.Stable.Rebuild, Tag: it.Tag, File: it.Bottle}
	}
	in.logf("==> Fetching %s", plural(len(reqs), "bottle"))
	fetched, err := in.Fetcher.FetchAll(ctx, reqs)
	if err != nil {
		return nil, err
	}
	for i, it := range plan.Items {
		if err := ctx.Err(); err != nil {
			return results, err
		}
		r, err := in.pour(it, fetched[i])
		if err != nil {
			return results, fmt.Errorf("%s: %w", it.Formula.Name, err)
		}
		results = append(results, r)
	}
	return results, nil
}

// lockAll takes the lock of every formula in name order, or none of them.
func (in *Installer) lockAll(items []Item) (unlock func() error, err error) {
	names := make([]string, len(items))
	for i, it := range items {
		names[i] = it.Formula.Name
	}
	slices.Sort(names)
	var held []*lock.Lock
	release := func() error {
		var errs []error
		for _, l := range slices.Backward(held) {
			errs = append(errs, l.Release())
		}
		return errors.Join(errs...)
	}
	for _, name := range names {
		l, err := lock.Acquire(in.Root, name)
		if err != nil {
			return nil, errors.Join(err, release())
		}
		held = append(held, l)
	}
	return release, nil
}

func (in *Installer) pour(it Item, res bottle.Result) (result Result, err error) {
	f := it.Formula
	in.logf("==> Pouring %s %s", f.Name, it.PkgVersion)

	dest := path.Join(fsx.Staging, randomID())
	defer func() {
		err = errors.Join(err, in.Root.RemoveAll(dest))
		_ = in.Root.Remove(fsx.Staging) // only succeeds when empty
	}()
	if err := bottle.Extract(in.Root, res.Path, dest, f.Name, it.PkgVersion); err != nil {
		return Result{}, err
	}
	staged := path.Join(dest, f.Name, it.PkgVersion)

	receipt, err := cellar.NewPourReceipt(res.Tab, cellar.Pour{
		OnRequest: it.OnRequest, Time: in.now(), Aliases: f.Aliases, Stable: f.Versions.Stable,
		Scheme: f.VersionScheme, Tap: f.Tap, Path: in.APIPath,
	})
	if err != nil {
		return Result{}, err
	}
	data, err := receipt.Marshal()
	if err != nil {
		return Result{}, err
	}
	if err := in.Root.WriteFile(path.Join(staged, "INSTALL_RECEIPT.json"), data, 0o644); err != nil {
		return Result{}, err
	}
	if err := in.relocate(it, staged, receipt); err != nil {
		return Result{}, err
	}

	rack := path.Join("Cellar", f.Name)
	madeRack := !in.exists(rack)
	if err := in.Root.MkdirAll(rack, 0o755); err != nil {
		return Result{}, err
	}
	keg := path.Join(rack, it.PkgVersion)
	if err := in.Root.Rename(staged, keg); err != nil {
		if madeRack {
			_ = in.Root.Remove(rack)
		}
		return Result{}, err
	}

	result = Result{Name: f.Name, Version: it.PkgVersion}
	if abs, err := in.Root.Abs(keg); err == nil {
		result.Size, _ = cellar.Keg{Path: abs}.Abv()
		in.logf("    %s: %s", abs, result.Size)
	}
	k := link.Keg{Name: f.Name, Version: it.PkgVersion, Aliases: receipt.Aliases}
	if it.KegOnly {
		result.LinkErr = in.Linker.Optlink(k)
	} else if result.LinkErr = in.Linker.Link(k); result.LinkErr == nil {
		result.Linked = true
	}
	if err := in.restoreEtcVar(keg); err != nil {
		result.PostInstallErr = err
		return result, nil
	}
	if len(f.PostInstallSteps) > 0 {
		in.logf("==> Running post_install for %s", f.Name)
		result.PostInstallErr = poststep.Run(poststep.Context{
			Root: in.Root, Prefix: in.Prefix, Name: f.Name, Version: f.Versions.Stable, Keg: keg,
			OS: cmp.Or(in.OS, runtime.GOOS), User: in.User, Log: in.Log,
		}, f.PostInstallSteps)
	}
	return result, nil
}

func (in *Installer) now() time.Time {
	if in.Now != nil {
		return in.Now()
	}
	return time.Now()
}

func (in *Installer) exists(p string) bool {
	_, err := in.Root.Lstat(p)
	return err == nil
}

// relocate fills the placeholders in the staged keg the way brew does: text
// files always, Mach-O files unless the bottle needs no relocation.
func (in *Installer) relocate(it Item, staged string, r *cellar.Receipt) error {
	loc := relocate.ForPrefix(in.Prefix)
	var deps []string
	for _, d := range r.RuntimeDependencies {
		deps = append(deps, path.Base(d.FullName))
	}
	loc.OpenJDK = relocate.OpenJDK(deps)
	if _, err := relocate.Text(in.Root, staged, r.ChangedFiles, loc); err != nil {
		return err
	}
	if it.Bottle.Cellar == ":any_skip_relocation" {
		return nil
	}
	modified, err := relocate.MachO(in.Root, staged, r.LinkageFiles, loc)
	if err != nil || len(modified) == 0 {
		return err
	}
	sign := in.Sign
	if sign == nil {
		if runtime.GOOS != "darwin" {
			return nil
		}
		sign = relocate.Codesign
	}
	return sign(in.Root, staged, modified)
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
