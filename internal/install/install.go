package install

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"runtime"
	"slices"
	"strings"
	"time"

	"github.com/jigsmiths/brw/internal/bottle"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
	"github.com/jigsmiths/brw/internal/poststep"
	"github.com/jigsmiths/brw/internal/relocate"
	"github.com/jigsmiths/brw/internal/sbom"
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
	// SBOMCreator names the tool in the SBOM of what it pours, for example
	// "Tool: https://github.com/jigsmiths/brw@abc1234".
	SBOMCreator string

	// KeepGoing records a formula's failure in its Result and carries on with the
	// next, as an upgrade of many formulae does.
	KeepGoing bool
	// After runs once a formula is installed, for the work that follows each one.
	After func(Item, Result)
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

	Upgraded    bool
	Reinstalled bool
	OldVersion  string // the version an upgrade or reinstall replaced
	// Err is why the formula was not installed, when the installer is told to
	// keep going after a failure.
	Err error
}

func (in *Installer) logf(format string, args ...any) {
	if in.Log != nil {
		_, _ = fmt.Fprintf(in.Log, format+"\n", args...)
	}
}

// Install pours each item in order. It locks every formula first and fetches
// all the bottles in parallel. If a formula fails, those before it stay
// installed and the ones after it are not touched, unless KeepGoing is set,
// which records the failure in the Result and carries on. A formula that cannot
// be linked stays poured and is reported in its Result.
func (in *Installer) Install(ctx context.Context, plan *Plan) (results []Result, err error) {
	if len(plan.Items) == 0 {
		return nil, nil
	}
	unlock, err := in.lockAll(plan)
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
	in.logf("==> Fetching downloads for: %s", sentence(reqs))
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
			err = fmt.Errorf("%s: %w", it.Formula.Name, err)
			if !in.KeepGoing {
				return results, err
			}
			results = append(results, Result{Name: it.Formula.Name, Version: it.PkgVersion, Err: err})
			continue
		}
		results = append(results, r)
		if in.After != nil {
			in.After(it, r)
		}
	}
	return results, nil
}

// lockAll takes the lock of every formula the plan names, in name order, or none
// of them. A plan without Locks locks its items.
func (in *Installer) lockAll(plan *Plan) (unlock func() error, err error) {
	names := slices.Clone(plan.Locks)
	if len(names) == 0 {
		for _, it := range plan.Items {
			names = append(names, it.Formula.Name)
		}
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
		Scheme: f.VersionScheme, Tap: f.Tap, Path: in.APIPath, Arch: it.Arch,
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
	if it.RuntimeDeps != nil {
		// brew rewrites the receipt once the bottle is relocated, with the
		// dependencies the formula has now.
		receipt.RuntimeDependencies = it.RuntimeDeps
		if data, err = receipt.Marshal(); err != nil {
			return Result{}, err
		}
		if err := in.Root.Replace(path.Join(staged, "INSTALL_RECEIPT.json"), data); err != nil {
			return Result{}, err
		}
	}
	if err := in.updateSBOM(staged, it, res, time.Unix(receipt.Time, 0)); err != nil {
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
	if old != nil {
		result.Upgraded, result.Reinstalled, result.OldVersion = !it.Reinstall, it.Reinstall, old.version
	}
	if abs, err := in.Root.Abs(keg); err == nil {
		result.Size, _ = cellar.Keg{Path: abs}.Abv()
		in.logf("    %s: %s", abs, result.Size)
	}
	k := link.Keg{Name: f.Name, Version: it.PkgVersion, Aliases: receipt.Aliases}
	link := !it.KegOnly
	if it.Linked != nil { // an upgrade does what the old keg had
		link = *it.Linked
	}
	if !link {
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

// oldKegs is what an upgrade or reinstall does with the kegs it replaces.
type oldKegs struct {
	version string
	restore func() // puts things back when the new keg could not be poured
	commit  func() // finishes with the old keg once the new one is in
}

// leaveOldKegs says an upgrade or reinstall is starting and unlinks the old
// kegs, so they cannot get in the way of the new one. A reinstall also sets the
// keg in use aside, to be deleted once the new one is in and put back if not.
func (in *Installer) leaveOldKegs(it Item) (*oldKegs, error) {
	f := it.Formula
	current := cellar.Prefix(in.Prefix).DefaultKeg(it.Upgrade)
	if it.Reinstall {
		in.logf("==> Reinstalling %s", f.Name)
	} else {
		in.logf("==> Upgrading %s", f.Name)
		in.logf("  %s -> %s", current.Version, it.PkgVersion)
	}
	keg := func(k cellar.Keg) link.Keg {
		lk := link.Keg{Name: k.Name, Version: k.Version}
		if k.Receipt != nil {
			lk.Aliases = k.Receipt.Aliases
		}
		return lk
	}
	wasLinked := it.Linked != nil && *it.Linked
	relink := func() {
		if !wasLinked {
			return
		}
		if err := in.Linker.Link(keg(current)); err != nil {
			in.logf("brw: could not link %s %s again: %v", f.Name, current.Version, err)
		}
	}
	for _, k := range it.Upgrade {
		if err := in.Linker.Unlink(keg(k)); err != nil {
			relink()
			return nil, fmt.Errorf("unlinking %s %s: %w", k.Name, k.Version, err)
		}
	}
	old := &oldKegs{version: current.Version, restore: relink, commit: func() {}}
	if !it.Reinstall {
		return old, nil
	}

	kegPath := path.Join("Cellar", f.Name, current.Version)
	backup := kegPath + ".reinstall"
	if in.exists(backup) { // left by an earlier run
		if err := in.Root.RemoveKeg(backup); err != nil {
			relink()
			return nil, fmt.Errorf("removing the old backup %s: %w", backup, err)
		}
	}
	if err := in.Root.Rename(kegPath, backup); err != nil {
		relink()
		return nil, fmt.Errorf("setting %s aside: %w", kegPath, err)
	}
	old.restore = func() {
		if err := in.Root.Rename(backup, kegPath); err != nil {
			in.logf("brw: could not put %s back: %v", kegPath, err)
			return
		}
		relink()
	}
	old.commit = func() {
		if err := in.Root.RemoveKeg(backup); err != nil {
			in.logf("brw: could not remove %s: %v; brw cleanup will", backup, err)
		}
	}
	return old, nil
}

// updateSBOM gives the SBOM in the staged keg the time and tool of this pour and
// the supplement of the bottle's manifest, as brew does. A bottle without an
// SBOM, or with one that cannot be read, is left as it is.
func (in *Installer) updateSBOM(staged string, it Item, res bottle.Result, when time.Time) error {
	file := path.Join(staged, sbom.File)
	data, err := in.Root.ReadFile(file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	out, ok := sbom.Update(data, sbom.Params{
		Created: when, Creator: cmp.Or(in.SBOMCreator, "Tool: https://github.com/jigsmiths/brw"),
		Supplement: res.SBOMSupplement, Tag: it.HostTag,
	})
	if !ok {
		return nil
	}
	return in.Root.Replace(file, out)
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

// sentence lists the names as brew does: "a", "a and b", "a, b and c".
func sentence(reqs []bottle.Request) string {
	names := make([]string, len(reqs))
	for i, r := range reqs {
		names[i] = r.Name
	}
	if len(names) < 2 {
		return strings.Join(names, "")
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func randomID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
