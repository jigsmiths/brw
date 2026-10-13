package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/install/installtest"
)

// freshDownload writes a cached bottle into the cache the command will use.
func downloads(t *testing.T, e *env) string {
	t.Helper()
	dir := filepath.Join(e.client.CacheDir, "downloads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCleanupRemovesOldKegsAndSaysWhatItFreed(t *testing.T) {
	e, prefix := linkStore(t)
	addVersion(t, prefix, "app", "1.0", "0.9")
	out, err := runCmd(t, e, "cleanup", "-prefix", prefix)
	if err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Cellar/app/0.9") || !exists(t, prefix, "Cellar/app/1.0") {
		t.Errorf("kegs wrong after the cleanup:\n%s", out)
	}
	if !strings.Contains(out, "Removing: "+filepath.Join(prefix, "Cellar/app/0.9")+"... (") ||
		!strings.Contains(out, "==> This operation has freed approximately ") {
		t.Errorf("output = %q", out)
	}
}

func TestCleanupDryRunChangesNothing(t *testing.T) {
	e, prefix := linkStore(t)
	addVersion(t, prefix, "app", "1.0", "0.9")
	before := tree(t, prefix)
	out, err := runCmd(t, e, "cleanup", "-prefix", prefix, "-n")
	if err != nil || !equal(before, tree(t, prefix)) {
		t.Fatalf("err = %v, tree changed = %v", err, !equal(before, tree(t, prefix)))
	}
	if !strings.Contains(out, "Would remove: "+filepath.Join(prefix, "Cellar/app/0.9")+" (") ||
		!strings.Contains(out, "==> This operation would free approximately ") {
		t.Errorf("output = %q", out)
	}
}

func TestCleanupOfANamedFormulaOnly(t *testing.T) {
	e, prefix := linkStore(t)
	addVersion(t, prefix, "app", "1.0", "0.9")
	addVersion(t, prefix, "vault", "1.0", "0.9")
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Cellar/app/0.9") || !exists(t, prefix, "Cellar/vault/0.9") {
		t.Error("only app should have been cleaned")
	}
	t.Setenv("HOMEBREW_NO_CLEANUP_FORMULAE", "vault,other")
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "vault"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, prefix, "Cellar/vault/0.9") || !strings.Contains(e.err.(*bytes.Buffer).String(), "Refusing to clean vault") {
		t.Errorf("a formula in HOMEBREW_NO_CLEANUP_FORMULAE must be left alone: %q", e.err.(*bytes.Buffer).String())
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if !exists(t, prefix, "Cellar/vault/0.9") {
		t.Error("a full cleanup also leaves it")
	}
}

func TestCleanupPrunesThePrefix(t *testing.T) {
	e, prefix := linkStore(t)
	if err := os.Symlink("../Cellar/gone/1.0/bin/gone", filepath.Join(prefix, "bin/gone")); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "cleanup", "-prefix", prefix, "--prune-prefix")
	if err != nil || exists(t, prefix, "bin/gone") || !strings.Contains(out, "Pruned 1 symbolic links from "+prefix) {
		t.Fatalf("out = %q, err = %v", out, err)
	}
	if strings.Contains(out, "Removing") {
		t.Errorf("--prune-prefix does nothing else: %q", out)
	}
}

func TestCleanupAlsoAutoremovesAndHonoursTheOptOut(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	e, prefix := installed(t)
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	out, err := runCmd(t, e, "cleanup", "-prefix", prefix)
	if err != nil || strings.Contains(out, "Autoremoving") || !exists(t, prefix, "Cellar/libx/2.0") {
		t.Fatalf("with the opt-out: %q, %v", out, err)
	}
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "")
	out, err = runCmd(t, e, "cleanup", "-prefix", prefix)
	if err != nil || !strings.Contains(out, "==> Autoremoving 1 unneeded formula:\nlibx\n") || exists(t, prefix, "Cellar/libx") {
		t.Errorf("without it: %q, %v", out, err)
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupRemovesStaleDownloadsAndLockFiles(t *testing.T) {
	e, prefix := linkStore(t)
	dir := downloads(t, e)
	stale := filepath.Join(dir, "app--0.5.arm64_tahoe.bottle.tar.gz")
	aged := filepath.Join(dir, "other--1.0.arm64_tahoe.bottle.tar.gz")
	for _, p := range []string{stale, aged} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-200 * 24 * time.Hour)
	if err := os.Chtimes(aged, old, old); err != nil {
		t.Fatal(err)
	}
	if !exists(t, prefix, "var/homebrew/locks/app.formula.lock") {
		t.Fatal("the install should have left a lock file")
	}
	out, err := runCmd(t, e, "cleanup", "-prefix", prefix)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{stale, aged} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s stayed", filepath.Base(p))
		}
	}
	if exists(t, prefix, "var/homebrew/locks/app.formula.lock") {
		t.Error("the lock file nobody holds stayed")
	}
	if !strings.Contains(out, "Removing: "+stale+"... (1B)") {
		t.Errorf("output = %q", out)
	}
}

func TestCleanupPruneOption(t *testing.T) {
	e, prefix := linkStore(t)
	dir := downloads(t, e)
	file := filepath.Join(dir, "other--1.0.arm64_tahoe.bottle.tar.gz")
	write := func() {
		if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write()
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Error("a fresh download of a formula the listing does not know should stay")
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "--prune=all"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("--prune=all should remove it")
	}
	for _, bad := range []string{"soon", "-3", "1.5"} {
		if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "--prune="+bad); err == nil || !strings.Contains(err.Error(), "--prune") {
			t.Errorf("--prune=%s: %v", bad, err)
		}
	}
}

func TestCleanupWithoutTheFormulaListing(t *testing.T) {
	e, prefix := linkStore(t)
	addVersion(t, prefix, "app", "1.0", "0.9")
	if err := os.Symlink("nowhere", filepath.Join(prefix, "bin/gone")); err != nil {
		t.Fatal(err)
	}
	e.client = &api.Client{URL: "http://127.0.0.1:1", CacheDir: t.TempDir()} // offline, nothing cached
	out, err := runCmd(t, e, "cleanup", "-prefix", prefix)
	if err != nil {
		t.Fatal(err)
	}
	if !exists(t, prefix, "Cellar/app/0.9") || !strings.Contains(e.err.(*bytes.Buffer).String(), "without the formula listing") {
		t.Errorf("old kegs need the listing: %q", e.err.(*bytes.Buffer).String())
	}
	if exists(t, prefix, "bin/gone") || !strings.Contains(out, "Pruned 1 symbolic links") {
		t.Errorf("pruning does not: %q", out)
	}
}

func TestCleanupJournalsItsChanges(t *testing.T) {
	e, prefix := linkStore(t)
	addVersion(t, prefix, "app", "1.0", "0.9")
	journal := filepath.Join(t.TempDir(), "journal")
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "-journal", journal); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(journal)
	if err != nil || !strings.Contains(string(b), "removekeg Cellar/app/0.9\n") {
		t.Errorf("journal = %q, %v", b, err)
	}
}

func TestCleanupScrubRemovesDownloadsOfWhatIsNotInstalled(t *testing.T) {
	e, prefix := linkStore(t)
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "versioned"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(downloads(t, e), "versioned--1.0.arm64_tahoe.bottle.tar.gz")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("the current version's download goes only with --scrub")
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "-s"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("-s should remove it")
	}
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "--scrub"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error("--scrub should remove it")
	}
}

func TestCleanupPrunePrefixDoesNothingElse(t *testing.T) {
	e, prefix := linkStore(t)
	addVersion(t, prefix, "app", "1.0", "0.9")
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "--prune-prefix"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, prefix, "Cellar/app/0.9") {
		t.Error("--prune-prefix removed an old keg")
	}
}

func TestCleanupOfANamedFormulaLeavesTheRestOfThePrefixAlone(t *testing.T) {
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "")
	e, prefix := installed(t)
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "1")
	if _, err := runCmd(t, e, "uninstall", "-prefix", prefix, "app"); err != nil { // libx is unneeded now
		t.Fatal(err)
	}
	t.Setenv("HOMEBREW_NO_AUTOREMOVE", "")
	if err := os.Symlink("nowhere", filepath.Join(prefix, "bin/gone")); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(prefix, "Cellar/.brw-staging/old")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "libx"); err != nil {
		t.Fatal(err)
	}
	if !exists(t, prefix, "Cellar/libx/2.0") || !exists(t, prefix, "bin/gone") || !exists(t, prefix, "Cellar/.brw-staging/old") {
		t.Error("naming a formula limits the cleanup to it: no autoremove, no pruning, no leftovers")
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Cellar/libx") || exists(t, prefix, "bin/gone") || exists(t, prefix, "Cellar/.brw-staging") {
		t.Error("a full cleanup does all of it")
	}
}

func TestCleanupSweepsDSStoreFiles(t *testing.T) {
	e, prefix := linkStore(t)
	file := filepath.Join(prefix, "share/.DS_Store")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix, "-n"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("a dry run removed it")
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(file); err == nil {
		t.Error(".DS_Store stayed")
	}
}

func TestCleanupUsesTheVersionSchemeOfTheFormula(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "renumbered", Version: "1.0", Files: []installtest.File{file("bin/renumbered", "r", 0o755)}}, nil,
		map[string]any{"version_scheme": 1})
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "renumbered"); err != nil {
		t.Fatal(err)
	}
	addVersion(t, prefix, "renumbered", "1.0", "9.0")
	receipt := filepath.Join(prefix, "Cellar/renumbered/9.0/INSTALL_RECEIPT.json")
	b, err := os.ReadFile(receipt)
	if err != nil || !strings.Contains(string(b), `"version_scheme": 1`) {
		t.Fatalf("receipt: %v %s", err, b)
	}
	if err := os.WriteFile(receipt, []byte(strings.Replace(string(b), `"version_scheme": 1`, `"version_scheme": 0`, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Cellar/renumbered/9.0") || !exists(t, prefix, "Cellar/renumbered/1.0") {
		t.Error("9.0 is from an older scheme than the formula has, so it is the old one")
	}
}

func TestCleanupLeavesRacksThatAreOnlyAnAliasInTheListing(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "a", 0o755)}}, nil,
		map[string]any{"aliases": []string{"oldname"}})
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	addVersion(t, prefix, "app", "1.0", "0.9")
	for _, v := range []string{"1.0", "0.9"} {
		if err := os.CopyFS(filepath.Join(prefix, "Cellar/oldname", v), os.DirFS(filepath.Join(prefix, "Cellar/app/1.0"))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if exists(t, prefix, "Cellar/app/0.9") || !exists(t, prefix, "Cellar/oldname/0.9") {
		t.Error("oldname is an alias in the listing, not a formula with a current version of its own")
	}
}

func TestCleanupGoesByTheCurrentBottleRebuild(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "rb", Version: "1.0", Files: []installtest.File{file("bin/rb", "r", 0o755)}}, nil,
		map[string]any{"bottle": map[string]any{"stable": map[string]any{"rebuild": 2, "files": map[string]any{}}}})
	e, prefix := s.env()
	dir := downloads(t, e)
	stale := filepath.Join(dir, "rb--1.0.arm64_tahoe.bottle.1.tar.gz")
	current := filepath.Join(dir, "rb--1.0.arm64_tahoe.bottle.2.tar.gz")
	for _, p := range []string{stale, current} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runCmd(t, e, "cleanup", "-prefix", prefix); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); err == nil {
		t.Error("the download of rebuild 1 stayed although the bottle is at rebuild 2")
	}
	if _, err := os.Stat(current); err != nil {
		t.Error("the download of the current rebuild went")
	}
}
