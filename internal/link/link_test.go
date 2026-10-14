package link

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// linkAll links the keg and returns what the prefix looks like afterwards.
func linkAll(t *testing.T, l *Linker, dir string, k Keg) []string {
	t.Helper()
	if err := l.Link(k); err != nil {
		t.Fatalf("Link(%s): %v", k.Name, err)
	}
	return links(t, dir)
}

func check(t *testing.T, got, want []string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got\n  %s\nwant\n  %s", join(got), join(want))
	}
}

func join(s []string) string {
	out := ""
	for i, e := range s {
		if i > 0 {
			out += "\n  "
		}
		out += e
	}
	return out
}

func TestLinkBinAndRecords(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "jq", "1.0", files{"bin/jq", "bin/helpers/inner", "sbin/jqd", ".DS_Store", "bin/.DS_Store"})
	check(t, linkAll(t, l, dir, k), []string{
		"bin/jq -> ../Cellar/jq/1.0/bin/jq",
		"opt/jq -> ../Cellar/jq/1.0",
		"sbin/jqd -> ../Cellar/jq/1.0/sbin/jqd",
		"var/homebrew/linked/jq -> ../../../Cellar/jq/1.0",
	})
	if !l.Linked(k) {
		t.Error("the keg is not linked after Link")
	}
}

func TestLinkEtcMakesDirectoriesAndLinksFiles(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{"etc/x.conf", "etc/x.d/one.conf", "etc/x.d/deeper/two.conf"})
	check(t, linkAll(t, l, dir, k), []string{
		"etc/x.conf -> ../Cellar/x/1.0/etc/x.conf",
		"etc/x.d/",
		"etc/x.d/deeper/",
		"etc/x.d/deeper/two.conf -> ../../../Cellar/x/1.0/etc/x.d/deeper/two.conf",
		"etc/x.d/one.conf -> ../../Cellar/x/1.0/etc/x.d/one.conf",
		"opt/x -> ../Cellar/x/1.0",
		"var/homebrew/linked/x -> ../../../Cellar/x/1.0",
	})
}

func TestLinkIncludeLinksDirectoriesWhole(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{"include/x.h", "include/x/sub.h", "include/postgresql@16/server/a.h"})
	check(t, linkAll(t, l, dir, k), []string{
		"include/postgresql@16/",
		"include/postgresql@16/server/",
		"include/postgresql@16/server/a.h -> ../../../Cellar/x/1.0/include/postgresql@16/server/a.h",
		"include/x -> ../Cellar/x/1.0/include/x",
		"include/x.h -> ../Cellar/x/1.0/include/x.h",
		"opt/x -> ../Cellar/x/1.0",
		"var/homebrew/linked/x -> ../../../Cellar/x/1.0",
	})
}

func TestLinkShare(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{
		"share/x/data.txt",                       // an ordinary directory: linked whole
		"share/man/man1/x.1",                     // in the list of shared directories: made, files linked
		"share/doc/x/README",                     // doc is shared, but doc/x is not
		"share/zsh/site-functions/_x",            // everything below zsh is made
		"share/fish/vendor_completions.d/x.fish", // and below fish
		"share/info/x.info",                      // info files are linked
		"share/info/dir",                         // the historical index is left alone
		"share/locale/locale.alias",              // so is this one
		"share/locale/de/LC_MESSAGES/x.mo",       // locale directories are made
		"share/icons/hicolor/48x48/apps/x.png",   // icons are made all the way down
		"share/icons/hicolor/icon-theme.cache",   // except the cache
		"share/lua/5.4/x.lua",                    // lua/ is made, lua itself is not matched
		"share/aclocal/x.m4",
	})
	check(t, linkAll(t, l, dir, k), []string{
		"opt/x -> ../Cellar/x/1.0",
		"share/aclocal/",
		"share/aclocal/x.m4 -> ../../Cellar/x/1.0/share/aclocal/x.m4",
		"share/doc/",
		"share/doc/x -> ../../Cellar/x/1.0/share/doc/x",
		"share/fish/",
		"share/fish/vendor_completions.d/",
		"share/fish/vendor_completions.d/x.fish -> ../../../Cellar/x/1.0/share/fish/vendor_completions.d/x.fish",
		"share/icons/",
		"share/icons/hicolor/",
		"share/icons/hicolor/48x48/",
		"share/icons/hicolor/48x48/apps/",
		"share/icons/hicolor/48x48/apps/x.png -> ../../../../../Cellar/x/1.0/share/icons/hicolor/48x48/apps/x.png",
		"share/info/",
		"share/info/x.info -> ../../Cellar/x/1.0/share/info/x.info",
		"share/locale/",
		"share/locale/de/",
		"share/locale/de/LC_MESSAGES/",
		"share/locale/de/LC_MESSAGES/x.mo -> ../../../../Cellar/x/1.0/share/locale/de/LC_MESSAGES/x.mo",
		"share/lua -> ../Cellar/x/1.0/share/lua",
		"share/man/",
		"share/man/man1/",
		"share/man/man1/x.1 -> ../../../Cellar/x/1.0/share/man/man1/x.1",
		"share/x -> ../Cellar/x/1.0/share/x",
		"share/zsh/",
		"share/zsh/site-functions/",
		"share/zsh/site-functions/_x -> ../../../Cellar/x/1.0/share/zsh/site-functions/_x",
		"var/homebrew/linked/x -> ../../../Cellar/x/1.0",
	})
}

func TestLinkLib(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{
		"lib/libx.dylib",
		"lib/charset.alias",
		"lib/x/plugins/p.so",                          // an ordinary directory: linked whole
		"lib/pkgconfig/x.pc",                          // made
		"lib/cmake/x/xConfig.cmake",                   // cmake is made, but cmake/x is not
		"lib/python3.12/site-packages/x/__init__.py",  // brew's patterns are prefixes: everything below is made
		"lib/python3.12/site-packages/x/__init__.pyc", // bytecode in site-packages is left out
		"lib/ruby/gems/x.gem",
		"lib/Rsomething/file", // brew's /^R/ rule, taken literally
		"lib/perl5/vendor_perl/X.pm",
		"lib/Foo.app/Contents/Info.plist", // application bundles are left out
	})
	check(t, linkAll(t, l, dir, k), []string{
		"lib/Rsomething/",
		"lib/Rsomething/file -> ../../Cellar/x/1.0/lib/Rsomething/file",
		"lib/cmake/",
		"lib/cmake/x -> ../../Cellar/x/1.0/lib/cmake/x",
		"lib/libx.dylib -> ../Cellar/x/1.0/lib/libx.dylib",
		"lib/perl5/",
		"lib/perl5/vendor_perl/",
		"lib/perl5/vendor_perl/X.pm -> ../../../Cellar/x/1.0/lib/perl5/vendor_perl/X.pm",
		"lib/pkgconfig/",
		"lib/pkgconfig/x.pc -> ../../Cellar/x/1.0/lib/pkgconfig/x.pc",
		"lib/python3.12/",
		"lib/python3.12/site-packages/",
		"lib/python3.12/site-packages/x/",
		"lib/python3.12/site-packages/x/__init__.py -> ../../../../Cellar/x/1.0/lib/python3.12/site-packages/x/__init__.py",
		"lib/ruby/",
		"lib/ruby/gems/",
		"lib/ruby/gems/x.gem -> ../../../Cellar/x/1.0/lib/ruby/gems/x.gem",
		"lib/x -> ../Cellar/x/1.0/lib/x",
		"opt/x -> ../Cellar/x/1.0",
		"var/homebrew/linked/x -> ../../../Cellar/x/1.0",
	})
}

func TestLinkFrameworks(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{"Frameworks/X.framework/Versions/A/X", "Frameworks/X.framework/Versions/Current -> A"})
	check(t, linkAll(t, l, dir, k), []string{
		"Frameworks/",
		"Frameworks/X.framework/",
		"Frameworks/X.framework/Versions/",
		"Frameworks/X.framework/Versions/A -> ../../../Cellar/x/1.0/Frameworks/X.framework/Versions/A",
		"Frameworks/X.framework/Versions/Current -> ../../../Cellar/x/1.0/Frameworks/X.framework/Versions/Current",
		"opt/x -> ../Cellar/x/1.0",
		"var/homebrew/linked/x -> ../../../Cellar/x/1.0",
	})
}

func TestLinkSkipsLinksThatAlreadyLeadWhereTheyShould(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{
		"lib/liba.dylib -> ../../../../opt/y/lib/liba.dylib", // points at another keg's opt path
		"lib/libb.dylib -> ../../../../lib/libb.dylib",       // points at where it would be linked
		"lib/libc.dylib -> libx.dylib",                       // an ordinary relative link
		"lib/libx.dylib",
	})
	got := linkAll(t, l, dir, k)
	check(t, got, []string{
		"lib/libc.dylib -> ../Cellar/x/1.0/lib/libc.dylib",
		"lib/libx.dylib -> ../Cellar/x/1.0/lib/libx.dylib",
		"opt/x -> ../Cellar/x/1.0",
		"var/homebrew/linked/x -> ../../../Cellar/x/1.0",
	})
}

func TestLinkConflictWithAFile(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{"bin/a", "bin/b", "bin/c", "lib/libx.dylib"})
	if err := os.WriteFile(filepath.Join(dir, "bin/b"), []byte("the user's"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := l.Link(k)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("error = %v, want a ConflictError", err)
	}
	if conflict.Dst != "bin/b" || conflict.Src != "Cellar/x/1.0/bin/b" || conflict.Owner != "" {
		t.Errorf("conflict = %+v", conflict)
	}
	// Nothing is left half linked, and the user's file is still theirs.
	check(t, links(t, dir), []string{"bin/b", "opt/x -> ../Cellar/x/1.0"})
	if b, _ := os.ReadFile(filepath.Join(dir, "bin/b")); string(b) != "the user's" {
		t.Errorf("the user's file was changed: %q", b)
	}
	if l.Linked(k) {
		t.Error("a failed link left a linked record")
	}
}

func TestLinkConflictWithAnotherKegsLink(t *testing.T) {
	l, dir := newLinker(t)
	other := install(t, dir, "other", "2.0", files{"bin/tool"})
	checkLink := linkAll(t, l, dir, other)
	_ = checkLink
	k := install(t, dir, "x", "1.0", files{"bin/aaa", "bin/tool"})
	err := l.Link(k)
	var conflict *ConflictError
	if !errors.As(err, &conflict) || conflict.Owner != "other" || conflict.Dst != "bin/tool" {
		t.Fatalf("error = %v, want a ConflictError naming other", err)
	}
	check(t, links(t, dir), []string{
		"bin/tool -> ../Cellar/other/2.0/bin/tool",
		"opt/other -> ../Cellar/other/2.0",
		"opt/x -> ../Cellar/x/1.0",
		"var/homebrew/linked/other -> ../../../Cellar/other/2.0",
	})
}

func TestLinkReplacesDanglingLinksAndAcceptsCorrectOnes(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{"bin/dangling", "bin/correct"})
	if err := os.Symlink("../Cellar/gone/1.0/bin/dangling", filepath.Join(dir, "bin/dangling")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/x/1.0/bin/correct", filepath.Join(dir, "bin/correct")); err != nil {
		t.Fatal(err)
	}
	check(t, linkAll(t, l, dir, k), []string{
		"bin/correct -> ../Cellar/x/1.0/bin/correct",
		"bin/dangling -> ../Cellar/x/1.0/bin/dangling",
		"opt/x -> ../Cellar/x/1.0",
		"var/homebrew/linked/x -> ../../../Cellar/x/1.0",
	})
}

func TestLinkTurnsASharedDirectoryLinkIntoADirectory(t *testing.T) {
	l, dir := newLinker(t)
	a := install(t, dir, "a", "1.0", files{"share/shared/a.txt", "share/shared/sub/a2.txt"})
	check(t, linkAll(t, l, dir, a), []string{
		"opt/a -> ../Cellar/a/1.0",
		"share/shared -> ../Cellar/a/1.0/share/shared",
		"var/homebrew/linked/a -> ../../../Cellar/a/1.0",
	})
	b := install(t, dir, "b", "1.0", files{"share/shared/b.txt", "share/shared/sub/b2.txt"})
	check(t, linkAll(t, l, dir, b), []string{
		"opt/a -> ../Cellar/a/1.0",
		"opt/b -> ../Cellar/b/1.0",
		"share/shared/",
		"share/shared/a.txt -> ../../Cellar/a/1.0/share/shared/a.txt",
		"share/shared/b.txt -> ../../Cellar/b/1.0/share/shared/b.txt",
		"share/shared/sub/",
		"share/shared/sub/a2.txt -> ../../../Cellar/a/1.0/share/shared/sub/a2.txt",
		"share/shared/sub/b2.txt -> ../../../Cellar/b/1.0/share/shared/sub/b2.txt",
		"var/homebrew/linked/a -> ../../../Cellar/a/1.0",
		"var/homebrew/linked/b -> ../../../Cellar/b/1.0",
	})
	// Unlinking one of them leaves the other's links, in a directory that stays.
	if err := l.Unlink(a); err != nil {
		t.Fatal(err)
	}
	check(t, links(t, dir), []string{
		"opt/a -> ../Cellar/a/1.0",
		"opt/b -> ../Cellar/b/1.0",
		"share/shared/",
		"share/shared/b.txt -> ../../Cellar/b/1.0/share/shared/b.txt",
		"share/shared/sub/",
		"share/shared/sub/b2.txt -> ../../../Cellar/b/1.0/share/shared/sub/b2.txt",
		"var/homebrew/linked/b -> ../../../Cellar/b/1.0",
	})
}

func TestLinkDoesNotExplodeALinkOutsideTheCellar(t *testing.T) {
	l, dir := newLinker(t)
	mustMkdir(t, filepath.Join(dir, "elsewhere/shared"))
	if err := os.Symlink("../elsewhere/shared", filepath.Join(dir, "share/shared")); err != nil {
		t.Fatal(err)
	}
	k := install(t, dir, "x", "1.0", files{"share/shared/x.txt"})
	var conflict *ConflictError
	if err := l.Link(k); !errors.As(err, &conflict) || conflict.Dst != "share/shared" {
		t.Errorf("error = %v, want a conflict on share/shared", err)
	}
	if target, _ := os.Readlink(filepath.Join(dir, "share/shared")); target != "../elsewhere/shared" {
		t.Errorf("the user's link was changed to %q", target)
	}
}

func TestLinkRemovesADanglingDirectoryLink(t *testing.T) {
	l, dir := newLinker(t)
	if err := os.Symlink("../Cellar/gone/1.0/share/shared", filepath.Join(dir, "share/shared")); err != nil {
		t.Fatal(err)
	}
	k := install(t, dir, "x", "1.0", files{"share/shared/x.txt"})
	check(t, linkAll(t, l, dir, k), []string{
		"opt/x -> ../Cellar/x/1.0",
		"share/shared -> ../Cellar/x/1.0/share/shared",
		"var/homebrew/linked/x -> ../../../Cellar/x/1.0",
	})
}

func TestLinkUnlinkLinkIsStable(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "x", "1.0", files{"bin/x", "etc/x.d/one", "include/x/a.h", "lib/pkgconfig/x.pc", "share/man/man1/x.1", "share/doc/x/README"})
	first := linkAll(t, l, dir, k)
	if err := l.Unlink(k); err != nil {
		t.Fatal(err)
	}
	if err := l.Link(k); err != nil {
		t.Fatal(err)
	}
	check(t, links(t, dir), first)
}

func TestLinkRefusesAKegThatHasARecord(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "jq", "1.8.2", files{"bin/jq"})
	if err := os.Symlink("../../../Cellar/jq/1.8.2", filepath.Join(dir, "var/homebrew/linked/jq")); err != nil {
		t.Fatal(err)
	}
	if err := l.Link(k); !errors.Is(err, ErrAlreadyLinked) {
		t.Errorf("error = %v, want ErrAlreadyLinked", err)
	}
	if got := links(t, dir); len(got) != 1 {
		t.Errorf("a refused link changed the prefix: %v", got)
	}
}

func TestForgetDropsTheOptRecordOfTheKegAndAnEmptyRack(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "jq", "1.0", files{"bin/jq"})
	linkAll(t, l, dir, k)
	if err := l.Unlink(k); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "Cellar/jq/1.0")); err != nil {
		t.Fatal(err)
	}
	if err := l.Forget(k); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "Cellar/jq")); !os.IsNotExist(err) {
		t.Errorf("the empty rack is still there: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "opt/jq")); !os.IsNotExist(err) {
		t.Errorf("opt/jq is still there: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "opt")); !os.IsNotExist(err) {
		t.Errorf("the empty opt directory is still there, brew removes it: %v", err)
	}
}

func TestForgetDropsTheVersionedAliasRecordsOfTheKeg(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "go", "1.27.2", files{"bin/go"})
	k.Aliases = []string{"go@1.27"}
	linkAll(t, l, dir, k)
	if err := l.Unlink(k); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "Cellar/go/1.27.2")); err != nil {
		t.Fatal(err)
	}
	if err := l.Forget(k); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "opt")); !os.IsNotExist(err) {
		got, _ := os.ReadDir(filepath.Join(dir, "opt"))
		t.Errorf("opt is still there with %d entries, such as the record of the alias: %v", len(got), err)
	}
}

func TestForgetKeepsWhatBelongsToOtherVersions(t *testing.T) {
	l, dir := newLinker(t)
	old := install(t, dir, "jq", "1.0", files{"bin/jq"})
	cur := install(t, dir, "jq", "2.0", files{"bin/jq"})
	other := install(t, dir, "tool", "1.0", files{"bin/tool"})
	linkAll(t, l, dir, other)
	linkAll(t, l, dir, cur) // opt/jq leads to 2.0
	if err := os.RemoveAll(filepath.Join(dir, "Cellar/jq/1.0")); err != nil {
		t.Fatal(err)
	}
	if err := l.Forget(old); err != nil {
		t.Fatal(err)
	}
	check(t, links(t, dir), []string{
		"bin/jq -> ../Cellar/jq/2.0/bin/jq",
		"bin/tool -> ../Cellar/tool/1.0/bin/tool",
		"opt/jq -> ../Cellar/jq/2.0",
		"opt/tool -> ../Cellar/tool/1.0",
		"var/homebrew/linked/jq -> ../../../Cellar/jq/2.0",
		"var/homebrew/linked/tool -> ../../../Cellar/tool/1.0",
	})
	// The opt record of the keg that is removed goes, though another version stays.
	if err := os.RemoveAll(filepath.Join(dir, "Cellar/jq/2.0")); err != nil {
		t.Fatal(err)
	}
	if err := l.Forget(cur); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "opt/jq")); !os.IsNotExist(err) {
		t.Errorf("opt/jq stayed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "opt/tool")); err != nil {
		t.Errorf("opt/tool went: %v", err)
	}
}

// symlinkCount is the number of symlinks in the prefix outside the Cellar.
func symlinkCount(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	for _, e := range links(t, dir) {
		if strings.Contains(e, " -> ") && !strings.HasPrefix(e, "Cellar/") {
			n++
		}
	}
	return n
}

func TestLinkCountMatchesTheLinksIntoThePrefix(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "jq", "1.0", files{"bin/jq", "lib/libjq.dylib", "lib/pkgconfig/jq.pc", "share/man/man1/jq.1", "include/jq.h", "etc/jq.conf"})
	before := symlinkCount(t, dir)
	n, err := l.LinkCount(k)
	if err != nil {
		t.Fatal(err)
	}
	// Like brew, the opt and linked records are not counted.
	if got := symlinkCount(t, dir) - before; n != got-2 || n < 5 {
		t.Errorf("LinkCount = %d, symlinks created = %d", n, got)
	}
	removed, err := l.UnlinkCount(k)
	if err != nil {
		t.Fatal(err)
	}
	if got := symlinkCount(t, dir); removed != n || got != before+1 { // the opt record stays
		t.Errorf("UnlinkCount = %d (want %d), %d symlinks left (want %d)", removed, n, got, before+1)
	}
}

func TestLinkCountLeavesOutLinksThatWereAlreadyRight(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "jq", "1.0", files{"bin/jq", "share/man/man1/jq.1"})
	if _, err := l.LinkCount(k); err != nil {
		t.Fatal(err)
	}
	if err := l.Root.Remove("var/homebrew/linked/jq"); err != nil { // links stay, the record goes
		t.Fatal(err)
	}
	if n, err := l.LinkCount(k); err != nil || n != 0 {
		t.Errorf("only the records were made again, and they are not counted: n = %d, err = %v", n, err)
	}
	if err := os.Remove(filepath.Join(dir, "bin/jq")); err != nil {
		t.Fatal(err)
	}
	if err := l.Root.Remove("var/homebrew/linked/jq"); err != nil {
		t.Fatal(err)
	}
	if n, err := l.LinkCount(k); err != nil || n != 1 {
		t.Errorf("one link was missing: n = %d, err = %v", n, err)
	}
}
