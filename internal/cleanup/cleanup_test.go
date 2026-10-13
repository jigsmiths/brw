package cleanup

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
)

var now = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

type fixture struct {
	t       *testing.T
	dir     string
	root    *fsx.Root
	out     bytes.Buffer
	warn    bytes.Buffer
	current map[string]Formula
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	for _, d := range []string{"Cellar", "opt", "bin", "etc", "include", "lib", "sbin", "share", "var/homebrew/linked", "var/homebrew/locks"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return &fixture{t: t, dir: dir, root: root, current: map[string]Formula{}}
}

// keg installs Cellar/name/version with one file, and links it if asked.
func (f *fixture) keg(name, version string, linked bool) { f.schemeKeg(name, version, 0, linked) }

func (f *fixture) schemeKeg(name, version string, scheme int, linked bool) {
	f.t.Helper()
	k := filepath.Join(f.dir, "Cellar", name, version)
	if err := os.MkdirAll(filepath.Join(k, "bin"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(k, "bin", name), []byte(name+version), 0o755); err != nil {
		f.t.Fatal(err)
	}
	receipt := fmt.Sprintf(`{"homebrew_version":"x","aliases":[],"source":{"tap":"homebrew/core","versions":{"version_scheme":%d}}}`, scheme)
	if err := os.WriteFile(filepath.Join(k, "INSTALL_RECEIPT.json"), []byte(receipt), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if linked {
		l := &link.Linker{Root: f.root, Prefix: f.dir}
		if err := l.Link(link.Keg{Name: name, Version: version}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) cleaner() *Cleaner {
	f.t.Helper()
	kegs, err := cellar.Prefix(f.dir).Kegs()
	if err != nil {
		f.t.Fatal(err)
	}
	return &Cleaner{
		Root: f.root, Prefix: f.dir, Linker: &link.Linker{Root: f.root, Prefix: f.dir}, Kegs: kegs,
		Current: func(name string) (Formula, bool) {
			c, ok := f.current[name]
			return c, ok
		},
		Now: func() time.Time { return now },
		Out: &f.out, Warn: &f.warn, PruneDays: 120,
	}
}

func (f *fixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.dir, rel))
	return err == nil
}

func (f *fixture) write(rel, body string) {
	f.t.Helper()
	p := filepath.Join(f.dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func TestOldKegsAreRemovedWhenTheCurrentVersionIsInstalled(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "1.1_1", false)
	f.keg("jq", "1.2", true)
	f.keg("tool", "3.0", true)
	f.current["jq"] = Formula{PkgVersion: "1.2"}
	f.current["tool"] = Formula{PkgVersion: "3.0"}
	c := f.cleaner()
	if err := c.OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq/1.0") || f.exists("Cellar/jq/1.1_1") || !f.exists("Cellar/jq/1.2") || !f.exists("Cellar/tool/3.0") {
		t.Errorf("kegs are wrong after the cleanup")
	}
	for _, name := range []string{"1.0", "1.1_1"} {
		if want := "Removing: " + filepath.Join(f.dir, "Cellar/jq", name) + "... ("; !strings.Contains(f.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, f.out.String())
		}
	}
	if c.Freed() == 0 {
		t.Error("nothing counted as freed")
	}
	if f.warn.Len() != 0 {
		t.Errorf("warnings = %q", f.warn.String())
	}
}

func TestOldKegsDryRunRemovesNothing(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "1.2", true)
	f.current["jq"] = Formula{PkgVersion: "1.2"}
	c := f.cleaner()
	c.DryRun = true
	if err := c.OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/jq/1.0") {
		t.Error("a dry run removed a keg")
	}
	if want := "Would remove: " + filepath.Join(f.dir, "Cellar/jq/1.0") + " ("; !strings.Contains(f.out.String(), want) {
		t.Errorf("output = %q", f.out.String())
	}
	if c.Freed() == 0 {
		t.Error("a dry run should count what it would free")
	}
}

func TestOldKegsAreKeptWhenTheCurrentVersionIsNotInstalled(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "1.1", true)
	f.current["jq"] = Formula{PkgVersion: "1.2"}
	if err := f.cleaner().OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/jq/1.0") || !f.exists("Cellar/jq/1.1") {
		t.Error("nothing is old until the current version is installed")
	}
	if want := "Skipping jq: most recent version 1.2 not installed"; !strings.Contains(f.warn.String(), want) {
		t.Errorf("warnings = %q", f.warn.String())
	}
}

func TestOldKegsKeepsLinkedPinnedAndNeededOnes(t *testing.T) {
	f := newFixture(t)
	f.keg("linked", "1.0", true) // the old one is the linked one
	f.keg("linked", "2.0", false)
	f.keg("pinned", "1.0", false)
	f.keg("pinned", "2.0", false)
	f.keg("needed", "1.0", false)
	f.keg("needed", "2.0", false)
	for _, n := range []string{"linked", "pinned", "needed"} {
		f.current[n] = Formula{PkgVersion: "2.0"}
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/pinned/1.0", filepath.Join(f.dir, "var/homebrew/pinned/pinned")); err != nil {
		t.Fatal(err)
	}
	f.write("Cellar/needed/1.0/.keepme", filepath.Join(f.dir, "etc")+"\n"+filepath.Join(f.dir, "no-such-thing")+"\n")
	if err := f.cleaner().OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Cellar/linked/1.0", "Cellar/pinned/1.0", "Cellar/needed/1.0"} {
		if !f.exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	w := f.warn.String()
	for _, want := range []string{
		"Skipping (old) " + filepath.Join(f.dir, "Cellar/linked/1.0") + " due to it being linked",
		"Skipping (old) " + filepath.Join(f.dir, "Cellar/pinned/1.0") + " due to it being pinned",
		"Skipping " + filepath.Join(f.dir, "Cellar/needed/1.0") + " as it is needed by " + filepath.Join(f.dir, "etc"),
	} {
		if !strings.Contains(w, want) {
			t.Errorf("warnings lack %q:\n%s", want, w)
		}
	}
}

func TestOldKegsFollowsTheVersionScheme(t *testing.T) {
	f := newFixture(t)
	f.keg("renumbered", "9.0", false) // an older scheme is old whatever the version says
	f.schemeKeg("renumbered", "1.0", 1, true)
	f.current["renumbered"] = Formula{PkgVersion: "1.0", Scheme: 1}
	if err := f.cleaner().OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/renumbered/9.0") || !f.exists("Cellar/renumbered/1.0") {
		t.Errorf("9.0 is from scheme 0 and should go, 1.0 is current")
	}
}

func TestOldKegsRemovesTheOptRecordOfWhatItRemoves(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "2.0", false)
	f.current["jq"] = Formula{PkgVersion: "2.0"}
	if err := os.Symlink("../Cellar/jq/1.0", filepath.Join(f.dir, "opt/jq")); err != nil {
		t.Fatal(err)
	}
	if err := f.cleaner().OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq/1.0") || f.exists("opt/jq") {
		t.Error("the old keg and the opt record that led to it should both go")
	}
}

func TestOldKegsOnlyForTheNamedFormulaeAndNotTheOnesToSkip(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"a", "b", "c"} {
		f.keg(n, "1.0", false)
		f.keg(n, "2.0", true)
		f.current[n] = Formula{PkgVersion: "2.0"}
	}
	c := f.cleaner()
	c.Skip = func(name string) bool { return name == "c" }
	if err := c.OldKegs([]string{"a", "c"}); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/a/1.0") || !f.exists("Cellar/b/1.0") || !f.exists("Cellar/c/1.0") {
		t.Error("only a should have been cleaned")
	}
	if !strings.Contains(f.warn.String(), "Refusing to clean c because it is listed in HOMEBREW_NO_CLEANUP_FORMULAE") {
		t.Errorf("warnings = %q", f.warn.String())
	}
	if err := c.OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/b/1.0") || !f.exists("Cellar/c/1.0") {
		t.Error("a full cleanup cleans all but the skipped formulae")
	}
}

func TestOldKegsNeedsTheFormulaListing(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "2.0", true)
	c := f.cleaner()
	c.Current = nil
	if err := c.OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/jq/1.0") || !strings.Contains(f.warn.String(), "without the formula listing") {
		t.Errorf("warnings = %q", f.warn.String())
	}
}

func TestOldKegsLeavesFormulaeTheListingDoesNotKnow(t *testing.T) {
	f := newFixture(t)
	f.keg("gone", "1.0", false)
	f.keg("gone", "2.0", true)
	if err := f.cleaner().OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/gone/1.0") {
		t.Error("a formula that is no longer listed has no current version to compare with")
	}
}

func TestOldKegsRefusesWhileTheFormulaIsLocked(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "2.0", true)
	f.current["jq"] = Formula{PkgVersion: "2.0"}
	held, err := lock.Acquire(f.root, "jq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
	if err := f.cleaner().OldKegs(nil); err == nil || !f.exists("Cellar/jq/1.0") {
		t.Errorf("err = %v; the keg should stay while the formula is locked", err)
	}
}

func TestOldKegsKeepsHeadKegs(t *testing.T) {
	f := newFixture(t)
	f.keg("tool", "HEAD-abc123", false)
	f.keg("tool", "2.0", true)
	f.current["tool"] = Formula{PkgVersion: "2.0"}
	if err := f.cleaner().OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/tool/HEAD-abc123") {
		t.Error("a HEAD keg is not old")
	}
}

func TestOldKegsNeverRemovesAKegFromANewerSchemeThanTheFormulaHas(t *testing.T) {
	f := newFixture(t)
	f.schemeKeg("odd", "1.0", 1, false) // scheme 1, though the formula is back on scheme 0
	f.keg("odd", "2.0", true)
	f.current["odd"] = Formula{PkgVersion: "2.0", Scheme: 0}
	if err := f.cleaner().OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/odd/1.0") {
		t.Error("a keg from a newer scheme is not older")
	}
}

func TestOldKegsStaysQuietAboutAPinnedFormulaWhoseCurrentVersionIsMissing(t *testing.T) {
	f := newFixture(t)
	f.keg("pinned", "1.0", true)
	f.current["pinned"] = Formula{PkgVersion: "2.0"}
	if err := os.MkdirAll(filepath.Join(f.dir, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/pinned/1.0", filepath.Join(f.dir, "var/homebrew/pinned/pinned")); err != nil {
		t.Fatal(err)
	}
	if err := f.cleaner().OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if f.warn.Len() != 0 {
		t.Errorf("a pinned formula is meant to be behind: %q", f.warn.String())
	}
}

// download writes a cached bottle file with an age in days.
func (f *fixture) download(dir, name string, ageDays int) string {
	f.t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("bottle"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	when := now.Add(-time.Duration(ageDays) * 24 * time.Hour)
	if err := os.Chtimes(p, when, when); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) downloadsCleaner() (*Cleaner, string) {
	f.t.Helper()
	dir := filepath.Join(f.t.TempDir(), "downloads")
	c := f.cleaner()
	c.Downloads = dir
	return c, dir
}

func has(t *testing.T, p string) bool {
	t.Helper()
	_, err := os.Lstat(p)
	return err == nil
}

func TestDownloadsOfOldVersionsAreStale(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.2", true)
	f.current["jq"] = Formula{PkgVersion: "1.2", Rebuild: 0}
	c, dir := f.downloadsCleaner()
	current := f.download(dir, "jq--1.2.arm64_tahoe.bottle.tar.gz", 1)
	currentTab := f.download(dir, "jq--1.2.arm64_tahoe.bottle.tab.json", 1)
	oldTar := f.download(dir, "jq--1.1.arm64_tahoe.bottle.tar.gz", 1)
	oldTab := f.download(dir, "jq--1.1.arm64_tahoe.bottle.tab.json", 1)
	rebuilt := f.download(dir, "jq--1.2.arm64_tahoe.bottle.1.tar.gz", 1) // a rebuild the formula no longer has
	unknown := f.download(dir, "gone--9.0.arm64_tahoe.bottle.tar.gz", 1) // not in the listing: left until it ages out
	foreign := f.download(dir, "notes.txt", 400)
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{oldTar, oldTab, rebuilt} {
		if has(t, p) {
			t.Errorf("%s should be gone", filepath.Base(p))
		}
	}
	for _, p := range []string{current, currentTab, unknown, foreign} {
		if !has(t, p) {
			t.Errorf("%s should stay", filepath.Base(p))
		}
	}
	if !strings.Contains(f.out.String(), "Removing: "+oldTar+"... (6B)") {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestDownloadsOlderThanTheLimitAreStaleAndTheLimitCanBeLifted(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.2", true)
	f.current["jq"] = Formula{PkgVersion: "1.2"}
	for days, wantGone := range map[int]bool{120: true, -1: false} {
		c, dir := f.downloadsCleaner()
		c.PruneDays = days
		old := f.download(dir, "jq--1.2.arm64_tahoe.bottle.tar.gz", 130)
		fresh := f.download(dir, "tool--1.0.arm64_tahoe.bottle.tar.gz", 10)
		if err := c.CleanDownloads(nil); err != nil {
			t.Fatal(err)
		}
		if has(t, old) == wantGone || !has(t, fresh) {
			t.Errorf("PruneDays %d: old exists = %v, fresh exists = %v", days, has(t, old), has(t, fresh))
		}
	}
	c, dir := f.downloadsCleaner()
	c.PruneDays = 0
	all := f.download(dir, "jq--1.2.arm64_tahoe.bottle.tar.gz", 0)
	other := f.download(dir, "notes.txt", 0)
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if has(t, all) || !has(t, other) {
		t.Errorf("prune all removes the bottles only: bottle exists = %v, other exists = %v", has(t, all), has(t, other))
	}
}

func TestScrubRemovesDownloadsOfWhatIsNotInstalled(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.2", true)
	f.current["jq"] = Formula{PkgVersion: "1.2"}
	f.current["tree"] = Formula{PkgVersion: "2.0"}
	c, dir := f.downloadsCleaner()
	installed := f.download(dir, "jq--1.2.arm64_tahoe.bottle.tar.gz", 1)
	notInstalled := f.download(dir, "tree--2.0.arm64_tahoe.bottle.tar.gz", 1)
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if !has(t, notInstalled) {
		t.Error("without --scrub the current version's download stays")
	}
	c.Scrub = true
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if has(t, notInstalled) || !has(t, installed) {
		t.Errorf("--scrub: not installed exists = %v, installed exists = %v", has(t, notInstalled), has(t, installed))
	}
}

func TestDownloadsOfInstalledVersionsStayEvenWhenOld(t *testing.T) {
	f := newFixture(t)
	f.keg("pinned", "1.0", true) // pinned or otherwise kept at an older version
	f.current["pinned"] = Formula{PkgVersion: "2.0"}
	c, dir := f.downloadsCleaner()
	old := f.download(dir, "pinned--1.0.arm64_tahoe.bottle.tar.gz", 1)
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if !has(t, old) {
		t.Error("the download of the installed version was removed")
	}
}

func TestIncompleteDownloadsAreRemovedOnceTheyAreOldEnoughToBeAbandoned(t *testing.T) {
	f := newFixture(t)
	c, dir := f.downloadsCleaner()
	abandoned := f.download(dir, ".incomplete-123", 1)
	tmp := f.download(dir, ".tmp-456", 1)
	running := f.download(dir, ".incomplete-789", 0)
	if err := os.Chtimes(running, now.Add(-time.Minute), now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if has(t, abandoned) || has(t, tmp) || !has(t, running) {
		t.Errorf("abandoned exists = %v, tmp exists = %v, running exists = %v", has(t, abandoned), has(t, tmp), has(t, running))
	}
}

func TestDownloadsDryRunAndMissingDirectory(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.2", true)
	f.current["jq"] = Formula{PkgVersion: "1.2"}
	c, dir := f.downloadsCleaner()
	old := f.download(dir, "jq--1.1.arm64_tahoe.bottle.tar.gz", 1)
	c.DryRun = true
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if !has(t, old) || !strings.Contains(f.out.String(), "Would remove: "+old+" (6B)") {
		t.Errorf("exists = %v, output = %q", has(t, old), f.out.String())
	}
	c.Downloads = filepath.Join(t.TempDir(), "never made")
	if err := c.CleanDownloads(nil); err != nil {
		t.Errorf("a missing cache is fine: %v", err)
	}
	c.Downloads = ""
	if err := c.CleanDownloads(nil); err != nil {
		t.Errorf("no cache directory: %v", err)
	}
}

func TestDownloadsWithoutTheListingOnlyGoByAge(t *testing.T) {
	f := newFixture(t)
	c, dir := f.downloadsCleaner()
	c.Current = nil
	oldVersion := f.download(dir, "jq--1.1.arm64_tahoe.bottle.tar.gz", 1)
	aged := f.download(dir, "jq--1.0.arm64_tahoe.bottle.tar.gz", 200)
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if !has(t, oldVersion) || has(t, aged) {
		t.Errorf("old version exists = %v, aged exists = %v", has(t, oldVersion), has(t, aged))
	}
}

func (f *fixture) age(rel string, minutes int) {
	f.t.Helper()
	when := now.Add(-time.Duration(minutes) * time.Minute)
	if err := os.Chtimes(filepath.Join(f.dir, rel), when, when); err != nil {
		f.t.Fatal(err)
	}
}

func TestLeftoversStagingAndReinstallKegs(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/.brw-staging/old/jq/1.0/bin/jq", "x")
	f.write("Cellar/.brw-staging/new/jq/1.0/bin/jq", "x")
	f.age("Cellar/.brw-staging/old", 120)
	f.age("Cellar/.brw-staging/new", 5)
	f.keg("jq", "1.0", true)
	f.write("Cellar/jq/1.0.reinstall/bin/jq", "x")
	if err := os.Chmod(filepath.Join(f.dir, "Cellar/jq/1.0.reinstall/bin"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := f.cleaner().Leftovers(); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/.brw-staging/old") || !f.exists("Cellar/.brw-staging/new") {
		t.Error("only the abandoned staging directory should go")
	}
	if f.exists("Cellar/jq/1.0.reinstall") || !f.exists("Cellar/jq/1.0") {
		t.Error("the reinstall keg should go and the keg stay")
	}
	if !strings.Contains(f.out.String(), "Removing: "+filepath.Join(f.dir, "Cellar/jq/1.0.reinstall")+"... (") {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestLeftoversRemovesTheStagingDirectoryOnceEmpty(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/.brw-staging/old/x", "x")
	f.age("Cellar/.brw-staging/old", 120)
	if err := f.cleaner().Leftovers(); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/.brw-staging") {
		t.Error("the empty staging directory stayed")
	}
}

func TestLeftoversDryRun(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/.brw-staging/old/x", "x")
	f.age("Cellar/.brw-staging/old", 120)
	f.keg("jq", "1.0", true)
	f.write("Cellar/jq/1.0.reinstall/x", "x")
	c := f.cleaner()
	c.DryRun = true
	if err := c.Leftovers(); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Cellar/.brw-staging/old") || !f.exists("Cellar/jq/1.0.reinstall") {
		t.Error("a dry run removed something")
	}
	if n := strings.Count(f.out.String(), "Would remove: "); n != 2 {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestLockFilesOfNobodyAreRemovedAndHeldOnesStay(t *testing.T) {
	f := newFixture(t)
	f.write("var/homebrew/locks/free.formula.lock", "")
	f.write("var/homebrew/locks/held.formula.lock", "")
	held, err := lock.Acquire(f.root, "held")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
	if err := os.Mkdir(filepath.Join(f.dir, "var/homebrew/locks/subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.cleaner().LockFiles(nil); err != nil {
		t.Fatal(err)
	}
	if f.exists("var/homebrew/locks/free.formula.lock") || !f.exists("var/homebrew/locks/held.formula.lock") || !f.exists("var/homebrew/locks/subdir") {
		t.Error("only the lock file nobody holds should go")
	}
	if f.out.Len() != 0 {
		t.Errorf("brew says nothing about lock files: %q", f.out.String())
	}
}

func TestLockFilesOfNamedFormulaeOnly(t *testing.T) {
	f := newFixture(t)
	f.write("var/homebrew/locks/a.formula.lock", "")
	f.write("var/homebrew/locks/b.formula.lock", "")
	if err := f.cleaner().LockFiles([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	if f.exists("var/homebrew/locks/a.formula.lock") || !f.exists("var/homebrew/locks/b.formula.lock") {
		t.Error("only a's lock file should go")
	}
}

func TestLockFilesAndDSStoreAreLeftAloneInADryRun(t *testing.T) {
	f := newFixture(t)
	f.write("var/homebrew/locks/free.formula.lock", "")
	f.write("share/.DS_Store", "")
	c := f.cleaner()
	c.DryRun = true
	if err := errors.Join(c.LockFiles(nil), c.DSStore(nil)); err != nil {
		t.Fatal(err)
	}
	if !f.exists("var/homebrew/locks/free.formula.lock") || !f.exists("share/.DS_Store") {
		t.Error("a dry run removed something")
	}
}

func TestDSStoreFilesGoFromTheStandardDirectories(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"bin/.DS_Store", "share/man/man1/.DS_Store", "Cellar/jq/1.0/.DS_Store", "opt/.DS_Store", "elsewhere/.DS_Store", "share/keep.txt"} {
		f.write(p, "")
	}
	if err := f.cleaner().DSStore(nil); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"bin/.DS_Store", "share/man/man1/.DS_Store", "Cellar/jq/1.0/.DS_Store", "opt/.DS_Store"} {
		if f.exists(p) {
			t.Errorf("%s stayed", p)
		}
	}
	if !f.exists("elsewhere/.DS_Store") || !f.exists("share/keep.txt") {
		t.Error("something outside the standard directories, or not a .DS_Store, was removed")
	}
}

func TestDSStoreOfNamedFormulaeOnlyLooksInTheirRacks(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/a/1.0/.DS_Store", "")
	f.write("Cellar/b/1.0/.DS_Store", "")
	f.write("bin/.DS_Store", "")
	if err := f.cleaner().DSStore([]string{"a"}); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/a/1.0/.DS_Store") || !f.exists("Cellar/b/1.0/.DS_Store") || !f.exists("bin/.DS_Store") {
		t.Error("only a's rack should be cleaned")
	}
}

func TestPrunePrefixRemovesBrokenLinksAndEmptyDirectories(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	for _, p := range []string{"share/man/empty/deeper/", "lib/pkgconfig/"} {
		if err := os.MkdirAll(filepath.Join(f.dir, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("../Cellar/gone/1.0/bin/gone", filepath.Join(f.dir, "bin/gone")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/gone/1.0/x", filepath.Join(f.dir, "share/man/empty/deeper/x")); err != nil {
		t.Fatal(err)
	}
	f.write("share/dirty/.DS_Store", "")
	c := f.cleaner()
	if err := c.PrunePrefix(); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"bin/gone", "share/man/empty", "share/dirty", "lib/pkgconfig"} {
		if f.exists(p) {
			t.Errorf("%s stayed", p)
		}
	}
	for _, p := range []string{"bin/jq", "bin", "share", "lib", "opt/jq", "var/homebrew/linked/jq"} {
		if !f.exists(p) {
			t.Errorf("%s was removed", p)
		}
	}
	if want := "Pruned 2 symbolic links and 5 directories from " + f.dir + "\n"; !strings.HasSuffix(f.out.String(), want) {
		t.Errorf("output = %q, want a final line %q", f.out.String(), want)
	}
}

func TestPrunePrefixSaysNothingWhenThereIsNothingToPrune(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	if err := f.cleaner().PrunePrefix(); err != nil || f.out.Len() != 0 {
		t.Errorf("output = %q, err = %v", f.out.String(), err)
	}
}

func TestPrunePrefixDryRunListsWhatWouldGo(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.dir, "share/a/b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(f.dir, "share/a/b/broken")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(f.dir, "bin/broken")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "lib/notempty"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.write("lib/notempty/file", "x")
	c := f.cleaner()
	c.DryRun = true
	if err := c.PrunePrefix(); err != nil {
		t.Fatal(err)
	}
	if !f.exists("bin/broken") || !f.exists("share/a/b/broken") {
		t.Error("a dry run removed something")
	}
	out := f.out.String()
	for _, want := range []string{
		"Would remove (broken link): " + filepath.Join(f.dir, "bin/broken"),
		"Would remove (broken link): " + filepath.Join(f.dir, "share/a/b/broken"),
		"Would remove (empty directory): " + filepath.Join(f.dir, "share/a/b"),
		"Would remove (empty directory): " + filepath.Join(f.dir, "share/a"),
	} {
		if !strings.Contains(out, want+"\n") {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "notempty") {
		t.Errorf("a directory with a file in it would not go:\n%s", out)
	}
}

func TestPrunePrefixLeavesWhatIsOutsideTheStandardDirectories(t *testing.T) {
	f := newFixture(t)
	if err := os.MkdirAll(filepath.Join(f.dir, "Caskroom/x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.dir, "Cellar/rack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("nowhere", filepath.Join(f.dir, "Caskroom/broken")); err != nil {
		t.Fatal(err)
	}
	if err := f.cleaner().PrunePrefix(); err != nil {
		t.Fatal(err)
	}
	if !f.exists("Caskroom/x") || !f.exists("Caskroom/broken") || !f.exists("Cellar/rack") {
		t.Error("only the standard directories are pruned")
	}
}

func TestDownloadsNewerThanTheListingAreKeptUnlessTheCurrentVersionIsInstalled(t *testing.T) {
	f := newFixture(t)
	f.current["tool"] = Formula{PkgVersion: "1.0"}
	c, dir := f.downloadsCleaner()
	newer := f.download(dir, "tool--1.5.arm64_tahoe.bottle.tar.gz", 1)
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if !has(t, newer) {
		t.Error("a download newer than the listing says is not out of date")
	}
	f.keg("tool", "1.0", true) // now the current version is installed
	c = f.cleaner()
	c.Downloads = dir
	if err := c.CleanDownloads(nil); err != nil {
		t.Fatal(err)
	}
	if has(t, newer) {
		t.Error("once the current version is installed, any other download is stale")
	}
}

func TestDownloadsOfNamedFormulaeOnly(t *testing.T) {
	f := newFixture(t)
	f.keg("a", "2.0", true)
	f.keg("b", "2.0", true)
	f.current["a"] = Formula{PkgVersion: "2.0"}
	f.current["b"] = Formula{PkgVersion: "2.0"}
	c, dir := f.downloadsCleaner()
	oldA := f.download(dir, "a--1.0.arm64_tahoe.bottle.tar.gz", 1)
	oldB := f.download(dir, "b--1.0.arm64_tahoe.bottle.tar.gz", 1)
	if err := c.CleanDownloads([]string{"A"}); err != nil {
		t.Fatal(err)
	}
	if has(t, oldA) || !has(t, oldB) {
		t.Errorf("a's old download exists = %v, b's = %v", has(t, oldA), has(t, oldB))
	}
}

func TestOldKegsCanBeRemovedByACallerThatHoldsTheLock(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "2.0", true)
	f.current["jq"] = Formula{PkgVersion: "2.0"}
	held, err := lock.Acquire(f.root, "jq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
	c := f.cleaner()
	c.LocksHeld = true
	if err := c.OldKegs(nil); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq/1.0") {
		t.Error("the caller holds the lock, so the cleaner does not need it")
	}
}
