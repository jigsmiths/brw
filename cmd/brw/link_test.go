package main

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/lock"
)

func kegOnly(reason string) map[string]any {
	return map[string]any{"keg_only": true, "keg_only_reason": map[string]any{"reason": reason, "explanation": ""}}
}

// linkStore has app (linked), plain (keg-only for a reason brew accepts a link for) and vault (keg-only and not versioned).
func linkStore(t *testing.T) (*env, string) {
	t.Helper()
	s := newStore(t)
	app := func(name string) installtest.Bottle {
		return installtest.Bottle{Name: name, Version: "1.0", Files: []installtest.File{file("bin/"+name, name, 0o755), file("share/man/man1/"+name+".1", name, 0)}}
	}
	s.formula(app("app"), nil, nil)
	s.formula(app("versioned"), nil, kegOnly(":versioned_formula"))
	s.formula(app("vault"), nil, kegOnly("it conflicts with something"))
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "app", "versioned", "vault"); err != nil {
		t.Fatal(err)
	}
	e.err.(*bytes.Buffer).Reset()
	return e, prefix
}

var countLine = regexp.MustCompile(`^Linking .*/Cellar/app/1\.0\.\.\. (\d+) symlinks created\.\n$`)

func TestUnlinkAndLinkAgain(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "unlink", "-prefix", prefix, "app")
	if err != nil || !regexp.MustCompile(`^Unlinking .*/Cellar/app/1\.0\.\.\. 2 symlinks removed\.\n$`).MatchString(out) {
		t.Fatalf("unlink: %q, %v", out, err)
	}
	for _, p := range []string{"bin/app", "var/homebrew/linked/app", "share/man/man1/app.1"} {
		if exists(t, prefix, p) {
			t.Errorf("%s is still there", p)
		}
	}
	if !exists(t, prefix, "opt/app") || !exists(t, prefix, "Cellar/app/1.0") {
		t.Error("unlink should leave the opt record and the keg")
	}
	out, err = runCmd(t, e, "link", "-prefix", prefix, "app")
	if m := countLine.FindStringSubmatch(out); err != nil || m == nil || m[1] != "2" {
		t.Fatalf("link: %q, %v", out, err)
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin/app")); got != "../Cellar/app/1.0/bin/app" {
		t.Errorf("bin/app -> %q", got)
	}
}

func TestLinkSaysWhenAlreadyLinked(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "link", "-prefix", prefix, "app")
	if err != nil || out != "To relink, run:\n  brw unlink app && brw link app\n" {
		t.Errorf("out = %q, err = %v", out, err)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "Already linked: "+filepath.Join(prefix, "Cellar/app/1.0")) {
		t.Errorf("stderr = %q", w)
	}
	if _, err := runCmd(t, e, "unlink", "-prefix", prefix, "vault"); err != nil {
		t.Fatal(err)
	}
	if _, err := runCmd(t, e, "link", "-prefix", prefix, "-f", "vault"); err != nil {
		t.Fatal(err)
	}
	out, err = runCmd(t, e, "link", "-prefix", prefix, "vault")
	if err != nil || !strings.Contains(out, "brw link --force vault") {
		t.Errorf("a keg-only hint needs --force: %q, %v", out, err)
	}
}

func TestLinkRefusesAKegOnlyFormulaWithoutForce(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "link", "-prefix", prefix, "vault")
	if err != nil {
		t.Fatalf("brew only warns: %v", err)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "vault is keg-only and must be linked with `--force`.") {
		t.Errorf("stderr = %q", w)
	}
	if !strings.Contains(out, filepath.Join(prefix, "opt/vault/bin")) || exists(t, prefix, "bin/vault") {
		t.Errorf("out = %q, linked = %v", out, exists(t, prefix, "bin/vault"))
	}
	out, err = runCmd(t, e, "link", "-prefix", prefix, "--force", "vault")
	if err != nil || !exists(t, prefix, "bin/vault") || !strings.Contains(out, "symlinks created.") {
		t.Errorf("--force: %q, %v", out, err)
	}
}

func TestLinkAllowsAVersionedKegOnlyFormulaWithoutForce(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "link", "-prefix", prefix, "versioned")
	if err != nil || !exists(t, prefix, "bin/versioned") || strings.Contains(out, "PATH") {
		t.Errorf("out = %q, err = %v", out, err)
	}
}

func TestLinkNeedsAListingToKnowAboutKegOnly(t *testing.T) {
	e, prefix := linkStore(t)
	e.client = &api.Client{URL: "http://127.0.0.1:1", CacheDir: t.TempDir()} // offline, nothing cached
	_, err := runCmd(t, e, "link", "-prefix", prefix, "vault")
	if err == nil || !strings.Contains(err.Error(), "--force") || exists(t, prefix, "bin/vault") {
		t.Fatalf("err = %v", err)
	}
	if _, err := runCmd(t, e, "link", "-prefix", prefix, "--force", "vault"); err != nil || !exists(t, prefix, "bin/vault") {
		t.Errorf("--force offline: %v", err)
	}
}

func TestProvidedByMacOS(t *testing.T) {
	for reason, want := range map[string]bool{
		":provided_by_macos": true, ":shadowed_by_macos": true, ":versioned_formula": false, "it conflicts with rust": false, "": false,
	} {
		if got := providedByMacOS(reason); got != want {
			t.Errorf("providedByMacOS(%q) = %v", reason, got)
		}
	}
}

func TestLinkRefusesMacOSSoftwareOnTheDefaultPrefix(t *testing.T) {
	s := newStore(t)
	s.formula(installtest.Bottle{Name: "zlib", Version: "1.0", Files: []installtest.File{file("lib/libz.a", "z", 0)}}, nil, kegOnly(":provided_by_macos"))
	e, prefix := s.env()
	if _, err := runCmd(t, e, "install", "-prefix", prefix, "zlib"); err != nil {
		t.Fatal(err)
	}
	e.err.(*bytes.Buffer).Reset()
	// On another prefix --force links it.
	if _, err := runCmd(t, e, "link", "-prefix", prefix, "--force", "zlib"); err != nil || !exists(t, prefix, "lib/libz.a") {
		t.Fatalf("--force on a non-default prefix: %v", err)
	}
	if _, err := runCmd(t, e, "unlink", "-prefix", prefix, "zlib"); err != nil {
		t.Fatal(err)
	}
	// On the default prefix not even --force does.
	e.defaultPrefix = prefix
	if _, err := runCmd(t, e, "link", "-prefix", prefix, "--force", "zlib"); err != nil || exists(t, prefix, "lib/libz.a") {
		t.Errorf("macOS software was linked: %v", err)
	}
	if w := e.err.(*bytes.Buffer).String(); !strings.Contains(w, "Refusing to link macOS provided/shadowed software: zlib") {
		t.Errorf("stderr = %q", w)
	}
}

func TestLinkAndUnlinkDryRunChangeNothing(t *testing.T) {
	e, prefix := linkStore(t)
	before := tree(t, prefix)
	out, err := runCmd(t, e, "unlink", "-prefix", prefix, "-n", "app")
	if err != nil || !strings.Contains(out, "Would unlink") || !strings.Contains(out, "would remove bin/app") {
		t.Fatalf("unlink -n: %q, %v", out, err)
	}
	if !equal(before, tree(t, prefix)) {
		t.Fatal("unlink -n changed the prefix")
	}
	if _, err := runCmd(t, e, "unlink", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	before = tree(t, prefix)
	out, err = runCmd(t, e, "link", "-prefix", prefix, "--dry-run", "app")
	if err != nil || !strings.Contains(out, "Would link") || !strings.Contains(out, "would symlink bin/app -> ../Cellar/app/1.0/bin/app") {
		t.Fatalf("link -n: %q, %v", out, err)
	}
	if !equal(before, tree(t, prefix)) {
		t.Error("link -n changed the prefix")
	}
}

func tree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(dir, p)
		if d.Type()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			rel += " -> " + target
		}
		out = append(out, rel)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func openRoot(t *testing.T, dir string) *fsx.Root {
	t.Helper()
	root, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root
}

func equal(a, b []string) bool { return strings.Join(a, "\n") == strings.Join(b, "\n") }

func TestLinkChecksEveryNameBeforeChangingAnything(t *testing.T) {
	e, prefix := linkStore(t)
	if _, err := runCmd(t, e, "unlink", "-prefix", prefix, "app"); err != nil {
		t.Fatal(err)
	}
	_, err := runCmd(t, e, "link", "-prefix", prefix, "app", "nosuchthing")
	if err == nil || !strings.Contains(err.Error(), `no available formula with the name "nosuchthing"`) || exists(t, prefix, "bin/app") {
		t.Errorf("err = %v, linked = %v", err, exists(t, prefix, "bin/app"))
	}
	if _, err := runCmd(t, e, "link", "-prefix", prefix); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("no names: %v", err)
	}
	if _, err := runCmd(t, e, "link", "-prefix", prefix, "--overwrite", "app"); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("--overwrite: %v", err)
	}
	if _, err := runCmd(t, e, "unlink", "-prefix", prefix); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Errorf("unlink with no names: %v", err)
	}
}

func TestLinkAndUnlinkTakeTheFormulaLock(t *testing.T) {
	e, prefix := linkStore(t)
	root := openRoot(t, prefix)
	held, err := lock.Acquire(root, "app")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
	if _, err := runCmd(t, e, "unlink", "-prefix", prefix, "app"); err == nil || !strings.Contains(err.Error(), "another process") {
		t.Errorf("unlink under a lock: %v", err)
	}
	if !exists(t, prefix, "bin/app") {
		t.Error("a locked formula was changed")
	}
}

func TestUnlinkWorksOnAnUnlinkedKegAndUsesTheOptKeg(t *testing.T) {
	e, prefix := linkStore(t)
	out, err := runCmd(t, e, "unlink", "-prefix", prefix, "vault")
	if err != nil || !strings.Contains(out, "0 symlinks removed.") {
		t.Errorf("a keg-only formula has nothing linked: %q, %v", out, err)
	}
}

// addVersion copies the keg of a formula to a newer version, which nothing links.
func addVersion(t *testing.T, prefix, name, from, to string) {
	t.Helper()
	if err := os.CopyFS(filepath.Join(prefix, "Cellar", name, to), os.DirFS(filepath.Join(prefix, "Cellar", name, from))); err != nil {
		t.Fatal(err)
	}
}

func TestUnlinkActsOnTheKegTheOptRecordNamesAndLinkOnTheLatest(t *testing.T) {
	e, prefix := linkStore(t)
	addVersion(t, prefix, "app", "1.0", "2.0")
	out, err := runCmd(t, e, "unlink", "-prefix", prefix, "app")
	if err != nil || !strings.Contains(out, "Cellar/app/1.0... 2 symlinks removed.") || exists(t, prefix, "bin/app") {
		t.Fatalf("unlink should act on the linked 1.0: %q, %v", out, err)
	}
	out, err = runCmd(t, e, "link", "-prefix", prefix, "app")
	if err != nil || !strings.Contains(out, "Cellar/app/2.0... ") {
		t.Fatalf("link should act on the latest, 2.0: %q, %v", out, err)
	}
	if got, _ := os.Readlink(filepath.Join(prefix, "bin/app")); got != "../Cellar/app/2.0/bin/app" {
		t.Errorf("bin/app -> %q", got)
	}
}
