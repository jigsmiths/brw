package uninstall

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
)

type fixture struct {
	t       *testing.T
	dir     string
	root    *fsx.Root
	linker  *link.Linker
	out     bytes.Buffer
	warn    bytes.Buffer
	known   map[string]string // alias or name -> name
	formula map[string]bool
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
	return &fixture{t: t, dir: dir, root: root, linker: &link.Linker{Root: root, Prefix: dir},
		known: map[string]string{}, formula: map[string]bool{}}
}

// keg installs a keg with a bin file and a receipt, and links it if asked.
func (f *fixture) keg(name, version string, linked bool, deps ...string) {
	f.t.Helper()
	k := filepath.Join(f.dir, "Cellar", name, version)
	if err := os.MkdirAll(filepath.Join(k, "bin"), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(k, "bin", name), []byte(name+version), 0o755); err != nil {
		f.t.Fatal(err)
	}
	var rd []string
	for _, d := range deps {
		rd = append(rd, `{"full_name":"`+d+`","version":"1","revision":0,"bottle_rebuild":0,"pkg_version":"1","declared_directly":true}`)
	}
	receipt := `{"homebrew_version":"x","runtime_dependencies":[` + strings.Join(rd, ",") + `],"aliases":[],"source":{"tap":"homebrew/core","versions":{"version_scheme":0}}}`
	if err := os.WriteFile(filepath.Join(k, "INSTALL_RECEIPT.json"), []byte(receipt), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.known[name] = name
	f.formula[name] = true
	if linked {
		if err := f.linker.Link(link.Keg{Name: name, Version: version}); err != nil {
			f.t.Fatal(err)
		}
	}
}

func (f *fixture) remover() *Remover {
	f.t.Helper()
	kegs, err := cellar.Prefix(f.dir).Kegs()
	if err != nil {
		f.t.Fatal(err)
	}
	return &Remover{
		Root: f.root, Prefix: f.dir, Linker: f.linker, Kegs: kegs, Out: &f.out, Warn: &f.warn,
		Resolve: func(n string) (string, bool) {
			c, ok := f.known[n]
			return c, ok
		},
		FormulaNames: func() map[string]bool { return f.formula },
	}
}

func (f *fixture) run(o Options, names ...string) error {
	f.t.Helper()
	return f.remover().Run(names, o)
}

func (f *fixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.dir, rel))
	return err == nil
}

// tree lists everything in the prefix except the lock files.
func (f *fixture) tree() []string {
	f.t.Helper()
	var out []string
	err := filepath.WalkDir(f.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(f.dir, p)
		if rel == "." || strings.HasPrefix(rel, "var/homebrew/locks") {
			return nil
		}
		if d.IsDir() {
			rel += "/"
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, _ := os.Readlink(p)
			rel += " -> " + target
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) kegPath(name, version string) string {
	return filepath.Join(f.dir, "Cellar", name, version)
}

func TestUninstallRemovesTheKegItsLinksAndRecords(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Cellar/jq", "bin/jq", "opt/jq", "var/homebrew/linked/jq", "opt"} {
		if f.exists(p) {
			t.Errorf("%s is still there", p)
		}
	}
	if got := f.out.String(); !strings.HasPrefix(got, "Uninstalling "+f.kegPath("jq", "1.0")+"... (2 files, ") || !strings.HasSuffix(got, ")\n") {
		t.Errorf("output = %q", got)
	}
}

func TestUninstallOfTheLinkedVersionLeavesTheOthersAndSaysSo(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "2.0", true)
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq/2.0") || !f.exists("Cellar/jq/1.0") {
		t.Error("the linked version should be the one that goes")
	}
	if f.exists("opt/jq") || f.exists("bin/jq") {
		t.Error("the records of the removed keg stayed")
	}
	want := "jq 1.0 is still installed.\nTo remove all versions, run:\n  brw uninstall --force jq\n"
	if !strings.HasSuffix(f.out.String(), want) {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestUninstallWithoutALinkedVersionTakesTheLatest(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.9", false)
	f.keg("jq", "1.10", false)
	f.keg("jq", "1.2", false)
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq/1.10") || !f.exists("Cellar/jq/1.9") || !f.exists("Cellar/jq/1.2") {
		t.Errorf("tree = %v", f.tree())
	}
	if !strings.Contains(f.out.String(), "jq 1.2 and 1.9 are still installed.") {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestUninstallForceRemovesEveryVersion(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", false)
	f.keg("jq", "2.0", true)
	if err := f.run(Options{Force: true}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq") || f.exists("bin/jq") || f.exists("opt/jq") {
		t.Errorf("tree = %v", f.tree())
	}
	if !strings.HasPrefix(f.out.String(), "Uninstalling jq... (4 files") || strings.Contains(f.out.String(), "still installed") {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestUninstallRefusesWhenAnInstalledFormulaNeedsTheKeg(t *testing.T) {
	f := newFixture(t)
	f.keg("oniguruma", "6.9", true)
	f.keg("jq", "1.0", true, "oniguruma")
	err := f.run(Options{}, "oniguruma")
	var de *DependentsError
	if !errors.As(err, &de) {
		t.Fatalf("err = %v", err)
	}
	want := "refusing to uninstall " + f.kegPath("oniguruma", "6.9") + "\nbecause it is required by jq, which is currently installed.\n" +
		"You can override this and force removal with:\n  brw uninstall --ignore-dependencies oniguruma"
	if err.Error() != want {
		t.Errorf("message = %q\nwant      %q", err.Error(), want)
	}
	if !f.exists("Cellar/oniguruma/6.9") || f.out.Len() != 0 {
		t.Error("something was removed")
	}
	if err := f.run(Options{IgnoreDependencies: true}, "oniguruma"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/oniguruma") {
		t.Error("--ignore-dependencies did not remove it")
	}
}

func TestUninstallOfBothADependencyAndItsDependentIsFine(t *testing.T) {
	f := newFixture(t)
	f.keg("oniguruma", "6.9", true)
	f.keg("jq", "1.0", true, "oniguruma")
	if err := f.run(Options{}, "oniguruma", "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/oniguruma") || f.exists("Cellar/jq") {
		t.Errorf("tree = %v", f.tree())
	}
}

func TestDependentsMessageNamesEveryone(t *testing.T) {
	e := &DependentsError{Kegs: []string{"/p/a/1", "/p/b/1"}, Dependents: []string{"x", "y", "z"}, Named: []string{"a", "b"}}
	want := "refusing to uninstall /p/a/1 and /p/b/1\nbecause they are required by x, y, and z, which are currently installed.\n" +
		"You can override this and force removal with:\n  brw uninstall --ignore-dependencies a b"
	if e.Error() != want {
		t.Errorf("message = %q", e.Error())
	}
}

func TestUninstallOnlyCountsKegsThatAreOptLinked(t *testing.T) {
	f := newFixture(t)
	f.keg("oniguruma", "6.9", false) // no opt record
	f.keg("jq", "1.0", true, "oniguruma")
	if err := f.run(Options{}, "oniguruma"); err != nil {
		t.Fatalf("a keg with no opt record is not a dependency: %v", err)
	}
}

func TestUninstallSkipsAPinnedFormulaWithoutFailing(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	if err := os.MkdirAll(filepath.Join(f.dir, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/jq/1.0", filepath.Join(f.dir, "var/homebrew/pinned/jq")); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatalf("brew exits 0 for a pinned formula: %v", err)
	}
	if !f.exists("Cellar/jq/1.0") || !f.exists("bin/jq") {
		t.Error("a pinned keg was removed")
	}
	if got := f.warn.String(); got != "brw: jq is pinned. You must unpin it to uninstall.\n" {
		t.Errorf("warning = %q", got)
	}
	if err := f.run(Options{Force: true}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq") || f.exists("var/homebrew/pinned") {
		t.Errorf("--force should remove the keg, the pin and the empty pin directory: %v", f.tree())
	}
}

func TestUninstallDropsThePinOfAnUnpinnedRemoval(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	f.keg("tool", "1.0", true)
	if err := os.MkdirAll(filepath.Join(f.dir, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/tool/1.0", filepath.Join(f.dir, "var/homebrew/pinned/tool")); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if !f.exists("var/homebrew/pinned/tool") {
		t.Error("another formula's pin was removed")
	}
}

func TestUninstallReportsWhatIsNotThere(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	f.known["tree"] = "tree" // a formula that is not installed
	err := f.run(Options{}, "nosuchthing", "tree", "jq")
	var un *UnavailableError
	var nk *NoSuchKegError
	if !errors.As(err, &un) || !errors.As(err, &nk) {
		t.Fatalf("err = %v", err)
	}
	if un.Error() != `no available formula with the name "nosuchthing"` || nk.Error() != "no such keg: "+filepath.Join(f.dir, "Cellar", "tree") {
		t.Errorf("messages: %q, %q", un, nk)
	}
	if f.exists("Cellar/jq") {
		t.Error("the formula that is installed should still be removed")
	}
	if err := newFixtureRun(t, Options{Force: true}, "nosuchthing"); err != nil {
		t.Errorf("--force ignores unknown names: %v", err)
	}
}

func newFixtureRun(t *testing.T, o Options, names ...string) error {
	t.Helper()
	return newFixture(t).run(o, names...)
}

func TestUninstallResolvesAliasesAndTapPrefix(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	f.known["jqalias"] = "jq"
	if err := f.run(Options{}, "JQALIAS"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq") {
		t.Error("the alias did not resolve")
	}
	f.keg("tree", "1.0", true)
	if err := f.run(Options{}, "homebrew/core/tree"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/tree") {
		t.Error("the tap prefix was not accepted")
	}
}

func TestUninstallWorksWithoutAFormulaListing(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	r := f.remover()
	r.Resolve, r.FormulaNames = nil, nil
	if err := r.Run([]string{"jq"}, Options{}); err != nil {
		t.Fatal(err)
	}
	r = f.remover()
	r.Resolve = nil
	err := r.Run([]string{"jq"}, Options{})
	var nk *NoSuchKegError
	if !errors.As(err, &nk) {
		t.Errorf("a missing keg with no listing = %v", err)
	}
}

func TestUninstallListsConfigurationFilesItLeaves(t *testing.T) {
	f := newFixture(t)
	f.keg("rustup", "1.0", true)
	f.keg("openssl@3", "3.0", true)
	f.formula["openssl@3"] = true
	for _, p := range []string{"etc/rustup/settings.toml", "etc/rustup-extra.conf", "etc/rustup.d/x", "etc/openssl@3/cert.pem"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(f.dir, p)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(f.dir, p), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.run(Options{}, "rustup"); err != nil {
		t.Fatal(err)
	}
	etc := func(p string) string { return filepath.Join(f.dir, "etc", p) }
	want := "brw: warning: The following rustup configuration files have not been removed!\nIf desired, remove them manually with `rm -rf`:\n  " +
		etc("rustup") + "\n  " + etc("rustup/settings.toml") + "\n" +
		"brw: warning: The following may be rustup configuration files and have not been removed!\nIf desired, remove them manually with `rm -rf`:\n  " +
		etc("rustup-extra.conf") + "\n  " + etc("rustup.d") + "\n"
	if f.warn.String() != want {
		t.Errorf("warnings =\n%s\nwant\n%s", f.warn.String(), want)
	}
	// A versioned formula looks for its unversioned name, and skips formulae.
	f.warn.Reset()
	if err := os.MkdirAll(filepath.Join(f.dir, "etc/openssl"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Options{}, "openssl@3"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(f.warn.String(), "etc/openssl@3/cert.pem\n  "+etc("openssl@3")) || !strings.Contains(f.warn.String(), etc("openssl")) {
		t.Errorf("warnings = %q", f.warn.String())
	}
}

func TestUninstallRemovesDanglingRackLinks(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	if err := os.Symlink("jq-gone", filepath.Join(f.dir, "Cellar/oldname")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("jq", filepath.Join(f.dir, "Cellar/jqlink")); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/oldname") || f.exists("Cellar/jqlink") {
		t.Errorf("tree = %v", f.tree())
	}
}

func TestUninstallDryRunChangesNothing(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	before := f.tree()
	r := f.remover()
	r.DryRun = true
	if err := r.Run([]string{"jq"}, Options{}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, f.tree()) {
		t.Error("a dry run changed the prefix")
	}
	if !strings.HasPrefix(f.out.String(), "Would uninstall "+f.kegPath("jq", "1.0")) {
		t.Errorf("output = %q", f.out.String())
	}
}

func TestUninstallRemovesKegsWithReadOnlyDirectories(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	if err := os.Chmod(filepath.Join(f.dir, "Cellar/jq/1.0/bin"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq") {
		t.Error("the keg stayed")
	}
}

func TestUninstallRefusesAFormulaThatIsBeingInstalled(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	held, err := lock.Acquire(f.root, "jq")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = held.Release() })
	err = f.run(Options{}, "jq")
	if !errors.Is(err, lock.ErrInProgress) {
		t.Fatalf("err = %v", err)
	}
	if !f.exists("Cellar/jq/1.0") || !f.exists("bin/jq") {
		t.Error("a locked formula was changed")
	}
}

func TestLatestOrdersVersionsLikeBrew(t *testing.T) {
	mk := func(vs ...string) []cellar.Keg {
		var out []cellar.Keg
		for _, v := range vs {
			out = append(out, cellar.Keg{Name: "x", Version: v})
		}
		return out
	}
	for _, tt := range []struct {
		versions []string
		want     string
	}{
		{[]string{"1.9", "1.10", "1.2"}, "1.10"},
		{[]string{"1.0", "1.0_1"}, "1.0_1"},
		{[]string{"2.0", "10.0", "9.9"}, "10.0"},
	} {
		if got := latest(mk(tt.versions...)).Version; got != tt.want {
			t.Errorf("latest(%v) = %s, want %s", tt.versions, got, tt.want)
		}
	}
}

func TestSentence(t *testing.T) {
	for in, want := range map[string]string{"a": "a", "a b": "a and b", "a b c": "a, b, and c"} {
		if got := sentence(strings.Fields(in)); got != want {
			t.Errorf("sentence(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUninstallTakesTheKegTheOptRecordNamesEvenWhenANewerOneIsInstalled(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true) // linked, and the older one
	f.keg("jq", "2.0", false)
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq/1.0") || !f.exists("Cellar/jq/2.0") {
		t.Errorf("tree = %v", f.tree())
	}
}

func TestUninstallFallsBackToTheLinkedRecord(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	f.keg("jq", "2.0", false)
	if err := os.Remove(filepath.Join(f.dir, "opt/jq")); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq/1.0") || !f.exists("Cellar/jq/2.0") {
		t.Errorf("tree = %v", f.tree())
	}
}

func TestUninstallRemovesAnEmptyPinDirectory(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	if err := os.MkdirAll(filepath.Join(f.dir, "var/homebrew/pinned"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("var/homebrew/pinned") {
		t.Error("brew removes the pin directory once it is empty")
	}
}

func TestUninstallPrefersTheOptRecordToTheLinkedRecord(t *testing.T) {
	f := newFixture(t)
	f.keg("jq", "1.0", true)
	f.keg("jq", "2.0", false)
	record := filepath.Join(f.dir, "var/homebrew/linked/jq")
	if err := os.Remove(record); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/jq/2.0", record); err != nil {
		t.Fatal(err)
	}
	if err := f.run(Options{}, "jq"); err != nil {
		t.Fatal(err)
	}
	if f.exists("Cellar/jq/1.0") || !f.exists("Cellar/jq/2.0") {
		t.Errorf("tree = %v", f.tree())
	}
}
