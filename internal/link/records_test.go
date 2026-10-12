package link

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestOptlink(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "jq", "1.8.2", files{"bin/jq"})
	if err := l.Optlink(k); err != nil {
		t.Fatal(err)
	}
	want := []string{"opt/jq -> ../Cellar/jq/1.8.2"}
	if got := links(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// Linking another version moves the record.
	k2 := install(t, dir, "jq", "1.9.0", files{"bin/jq"})
	if err := l.Optlink(k2); err != nil {
		t.Fatal(err)
	}
	if got := links(t, dir); !reflect.DeepEqual(got, []string{"opt/jq -> ../Cellar/jq/1.9.0"}) {
		t.Errorf("after the second keg: %v", got)
	}
	// And doing it again changes nothing.
	if err := l.Optlink(k2); err != nil {
		t.Fatal(err)
	}
	if got := links(t, dir); !reflect.DeepEqual(got, []string{"opt/jq -> ../Cellar/jq/1.9.0"}) {
		t.Errorf("after repeating: %v", got)
	}
}

func TestOptlinkReplacesWhateverIsThere(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "jq", "1.8.2", files{"bin/jq"})
	if err := os.WriteFile(filepath.Join(dir, "opt/jq"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := l.Optlink(k); err != nil {
		t.Fatal(err)
	}
	if got := links(t, dir); !reflect.DeepEqual(got, []string{"opt/jq -> ../Cellar/jq/1.8.2"}) {
		t.Errorf("got %v", got)
	}
	if err := os.Remove(filepath.Join(dir, "opt/jq")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "opt/jq"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Optlink(k); err != nil {
		t.Fatalf("an empty directory in the way: %v", err)
	}
}

func TestOptlinkAliasesAndRenamedRecords(t *testing.T) {
	l, dir := newLinker(t)
	old := install(t, dir, "openssl@3", "3.6.4", files{"bin/openssl"})
	if err := os.Symlink("../Cellar/openssl@3/3.6.4", filepath.Join(dir, "opt/openssl@3")); err != nil {
		t.Fatal(err)
	}
	// A record from an earlier name or alias that points into the same rack.
	if err := os.Symlink("../Cellar/openssl@3/3.6.4", filepath.Join(dir, "opt/openssl@3.6")); err != nil {
		t.Fatal(err)
	}
	// A record of another formula, which must stay.
	other := install(t, dir, "zlib", "1.3", files{"lib/libz.a"})
	if err := os.Symlink("../Cellar/zlib/1.3", filepath.Join(dir, "opt/zlib")); err != nil {
		t.Fatal(err)
	}
	_ = old
	_ = other

	k := install(t, dir, "openssl@3", "3.6.5", files{"bin/openssl"})
	k.Aliases = []string{"openssl@3.6", "ssl"}
	if err := l.Optlink(k); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"opt/openssl@3 -> ../Cellar/openssl@3/3.6.5",
		"opt/openssl@3.6 -> ../Cellar/openssl@3/3.6.5",
		"opt/ssl -> ../Cellar/openssl@3/3.6.5",
		"opt/zlib -> ../Cellar/zlib/1.3",
	}
	if got := links(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%v\nwant\n%v", got, want)
	}
}

func TestLinkedRecord(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "jq", "1.8.2", files{"bin/jq"})
	if l.Linked(k) {
		t.Error("a keg with no record is linked")
	}
	if err := os.Symlink("../../../Cellar/jq/1.8.2", filepath.Join(dir, "var/homebrew/linked/jq")); err != nil {
		t.Fatal(err)
	}
	if !l.Linked(k) {
		t.Error("the record was not seen")
	}
	if l.Linked(Keg{Name: "jq", Version: "1.9.0"}) {
		t.Error("a record for another version counts")
	}
	if err := os.Remove(filepath.Join(dir, "var/homebrew/linked/jq")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/gone/1.0", filepath.Join(dir, "var/homebrew/linked/jq")); err != nil {
		t.Fatal(err)
	}
	if l.Linked(k) {
		t.Error("a dangling record counts")
	}
}

func TestUnlinkRemovesOnlyLinksIntoTheKeg(t *testing.T) {
	l, dir := newLinker(t)
	a := install(t, dir, "a", "1.0", files{"bin/a", "lib/liba.dylib", "share/man/man1/a.1", "include/a.h"})
	b := install(t, dir, "b", "1.0", files{"bin/b", "share/man/man1/b.1"})
	for _, e := range []struct{ link, target string }{
		{"bin/a", "../Cellar/a/1.0/bin/a"},
		{"lib/liba.dylib", "../Cellar/a/1.0/lib/liba.dylib"},
		{"include/a.h", "../Cellar/a/1.0/include/a.h"},
		{"bin/b", "../Cellar/b/1.0/bin/b"},
		{"share/man/man1/a.1", "../../../Cellar/a/1.0/share/man/man1/a.1"},
		{"share/man/man1/b.1", "../../../Cellar/b/1.0/share/man/man1/b.1"},
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, e.link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(e.target, filepath.Join(dir, e.link)); err != nil {
			t.Fatal(err)
		}
	}
	// A file of the user's, a link into a lookalike path, and the records.
	if err := os.WriteFile(filepath.Join(dir, "bin/mine"), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(dir, "bin/other")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../Cellar/a/1.0", filepath.Join(dir, "var/homebrew/linked/a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Cellar/a/1.0", filepath.Join(dir, "opt/a")); err != nil {
		t.Fatal(err)
	}
	_ = b

	if err := l.Unlink(a); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"bin/b -> ../Cellar/b/1.0/bin/b",
		"bin/mine",
		"bin/other -> elsewhere",
		"opt/a -> ../Cellar/a/1.0", // unlink leaves the opt record, as brew does
		"share/man/",
		"share/man/man1/",
		"share/man/man1/b.1 -> ../../../Cellar/b/1.0/share/man/man1/b.1",
	}
	if got := links(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("got\n%v\nwant\n%v", got, want)
	}
}

func TestUnlinkLeavesLinksOfOtherKegsAtTheSamePath(t *testing.T) {
	l, dir := newLinker(t)
	a := install(t, dir, "a", "1.0", files{"bin/tool", "bin/mine"})
	install(t, dir, "b", "1.0", files{"bin/tool"})
	if err := os.Symlink("../Cellar/b/1.0/bin/tool", filepath.Join(dir, "bin/tool")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin/mine"), []byte("user file"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := l.Unlink(a); err != nil {
		t.Fatal(err)
	}
	want := []string{"bin/mine", "bin/tool -> ../Cellar/b/1.0/bin/tool"}
	if got := links(t, dir); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v: another keg's link and a regular file must stay", got, want)
	}
}

func TestUnlinkRemovesEmptiedDirectoriesButNotTheStandardOnes(t *testing.T) {
	l, dir := newLinker(t)
	a := install(t, dir, "a", "1.0", files{"share/doc/a/README", "lib/pkgconfig/a.pc"})
	for _, e := range []struct{ link, target string }{
		{"share/doc/a/README", "../../../Cellar/a/1.0/share/doc/a/README"},
		{"lib/pkgconfig/a.pc", "../../Cellar/a/1.0/lib/pkgconfig/a.pc"},
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, e.link)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(e.target, filepath.Join(dir, e.link)); err != nil {
			t.Fatal(err)
		}
	}
	if err := l.Unlink(a); err != nil {
		t.Fatal(err)
	}
	if got := links(t, dir); len(got) != 0 {
		t.Errorf("left behind: %v", got)
	}
	for _, d := range []string{"bin", "etc", "include", "lib", "sbin", "share", "opt", "var/homebrew/linked"} {
		if _, err := os.Stat(filepath.Join(dir, d)); err != nil {
			t.Errorf("the standard directory %s was removed: %v", d, err)
		}
	}
}

func TestUnlinkOfAKegThatWasNeverLinked(t *testing.T) {
	l, dir := newLinker(t)
	k := install(t, dir, "a", "1.0", files{"bin/a"})
	if err := l.Unlink(k); err != nil {
		t.Errorf("Unlink = %v", err)
	}
}
