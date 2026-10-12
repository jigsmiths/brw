package relocate

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/fsx"
)

var loc = Locations{
	Prefix: "/opt/brw", Cellar: "/opt/brw/Cellar", Repository: "/opt/brw", Library: "/opt/brw/Library",
}

const keg = fsx.Staging + "/id/jq/1.0"

// stage makes a prefix with a staged keg holding the given files (name to content).
func stage(t *testing.T, files map[string]string) (*fsx.Root, string) {
	t.Helper()
	dir := t.TempDir()
	r, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	if err := r.MkdirAll(keg, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := r.MkdirAll(filepath.Join(keg, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := r.WriteFile(filepath.Join(keg, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return r, dir
}

func read(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, keg, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestTextMatchesBrewOnARealBottle(t *testing.T) {
	bottle, err := os.ReadFile("testdata/libjq.pc.bottle")
	if err != nil {
		t.Fatal(err)
	}
	brewResult, err := os.ReadFile("testdata/libjq.pc.brew")
	if err != nil {
		t.Fatal(err)
	}
	want := strings.ReplaceAll(string(brewResult), "@@ORACLE@@", "/opt/brw")
	r, dir := stage(t, map[string]string{"lib/pkgconfig/libjq.pc": string(bottle)})
	changed, err := Text(r, keg, []string{"lib/pkgconfig/libjq.pc"}, loc)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, "lib/pkgconfig/libjq.pc"); got != want {
		t.Errorf("got\n%s\nwant brew's\n%s", got, want)
	}
	if !reflect.DeepEqual(changed, []string{"lib/pkgconfig/libjq.pc"}) {
		t.Errorf("changed = %v", changed)
	}
}

func TestTextReplacesEveryPlaceholder(t *testing.T) {
	r, dir := stage(t, map[string]string{"bin/tool": strings.Join([]string{
		"#!@@HOMEBREW_PERL@@ -w",
		"p=@@HOMEBREW_PREFIX@@/etc c=@@HOMEBREW_CELLAR@@/x r=@@HOMEBREW_REPOSITORY@@ l=@@HOMEBREW_LIBRARY@@/Homebrew",
		"j=@@HOMEBREW_JAVA@@ again=@@HOMEBREW_PREFIX@@/a:@@HOMEBREW_PREFIX@@/b",
	}, "\n")})
	withJava := loc
	withJava.OpenJDK = "openjdk@21"
	if _, err := Text(r, keg, []string{"bin/tool"}, withJava); err != nil {
		t.Fatal(err)
	}
	want := strings.Join([]string{
		"#!/opt/brw/opt/perl/bin/perl -w",
		"p=/opt/brw/etc c=/opt/brw/Cellar/x r=/opt/brw l=/opt/brw/Library/Homebrew",
		"j=/opt/brw/opt/openjdk@21/libexec again=/opt/brw/a:/opt/brw/b",
	}, "\n")
	if got := read(t, dir, "bin/tool"); got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
}

func TestTextLeavesJavaWithoutAnOpenJDKDependency(t *testing.T) {
	r, dir := stage(t, map[string]string{"f": "j=@@HOMEBREW_JAVA@@ p=@@HOMEBREW_PREFIX@@"})
	if _, err := Text(r, keg, []string{"f"}, loc); err != nil {
		t.Fatal(err)
	}
	if got := read(t, dir, "f"); got != "j=@@HOMEBREW_JAVA@@ p=/opt/brw" {
		t.Errorf("got %q", got)
	}
}

func TestTextOnlyRewritesChangedFiles(t *testing.T) {
	r, dir := stage(t, map[string]string{"a": "@@HOMEBREW_PREFIX@@", "b": "no placeholder here", "c": "@@HOMEBREW_PREFIX@@"})
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := r.Chtimes(keg+"/b", old, old); err != nil {
		t.Fatal(err)
	}
	changed, err := Text(r, keg, []string{"a", "b"}, loc) // c is not listed
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(changed, []string{"a"}) {
		t.Errorf("changed = %v, want only a", changed)
	}
	if read(t, dir, "c") != "@@HOMEBREW_PREFIX@@" {
		t.Error("a file that was not listed was rewritten")
	}
	if fi, _ := os.Stat(filepath.Join(dir, keg, "b")); !fi.ModTime().Equal(old) {
		t.Error("a file with nothing to replace was rewritten")
	}
}

func TestTextKeepsModesAndHardLinks(t *testing.T) {
	r, dir := stage(t, map[string]string{"bin/one": "@@HOMEBREW_PREFIX@@"})
	if err := r.Chmod(keg+"/bin/one", 0o555); err != nil {
		t.Fatal(err)
	}
	if err := r.Link(keg+"/bin/one", keg+"/bin/two"); err != nil {
		t.Fatal(err)
	}
	if err := r.WriteFile(keg+"/bin/other", []byte("@@HOMEBREW_PREFIX@@"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := Text(r, keg, []string{"bin/one", "bin/other", "bin/two"}, loc)
	if err != nil {
		t.Fatal(err)
	}
	// A group of hard links is reported together, in order of the group's first file.
	if want := []string{"bin/one", "bin/two", "bin/other"}; !reflect.DeepEqual(changed, want) {
		t.Errorf("changed = %v, want %v", changed, want)
	}
	for _, name := range []string{"bin/one", "bin/two"} {
		if got := read(t, dir, name); got != "/opt/brw" {
			t.Errorf("%s = %q", name, got)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, keg, "bin/one")); fi.Mode().Perm() != 0o555 {
		t.Errorf("mode = %v, want 0555 kept", fi.Mode().Perm())
	}
	a, _ := os.Stat(filepath.Join(dir, keg, "bin/one"))
	b, _ := os.Stat(filepath.Join(dir, keg, "bin/two"))
	if !os.SameFile(a, b) {
		t.Error("the hard links were split")
	}
}

func TestTextSkipsWhatItShould(t *testing.T) {
	r, dir := stage(t, map[string]string{"real": "@@HOMEBREW_PREFIX@@", "sub/f": "@@HOMEBREW_PREFIX@@"})
	secret := filepath.Join(dir, "outside")
	if err := os.WriteFile(secret, []byte("@@HOMEBREW_PREFIX@@"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := r.Symlink("real", keg+"/link"); err != nil {
		t.Fatal(err)
	}
	if err := r.Symlink("sub", keg+"/dirlink"); err != nil {
		t.Fatal(err)
	}
	if err := r.Mkdir(keg+"/adir", 0o755); err != nil {
		t.Fatal(err)
	}
	files := []string{
		"link",                   // a symlink
		"dirlink/f",              // through a symlinked directory
		"adir",                   // a directory
		"missing",                // not there
		"../../../../../outside", // climbs out of the keg
		"/etc/passwd",            // absolute
		"",                       // empty
	}
	changed, err := Text(r, keg, files, loc)
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 {
		t.Errorf("changed = %v, want nothing", changed)
	}
	for _, name := range []string{"real", "sub/f"} {
		if got := read(t, dir, name); got != "@@HOMEBREW_PREFIX@@" {
			t.Errorf("%s was rewritten through a symlink: %q", name, got)
		}
	}
	if b, _ := os.ReadFile(secret); string(b) != "@@HOMEBREW_PREFIX@@" {
		t.Error("a file outside the keg was rewritten")
	}
}

func TestForPrefix(t *testing.T) {
	clone := t.TempDir()
	if err := os.MkdirAll(filepath.Join(clone, "Library", "Homebrew"), 0o755); err != nil {
		t.Fatal(err)
	}
	intel := t.TempDir()
	if err := os.MkdirAll(filepath.Join(intel, "Homebrew", "Library"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tt := range map[string]struct{ prefix, repo string }{
		"a prefix that is the repository": {clone, clone},
		"a repository below the prefix":   {intel, filepath.Join(intel, "Homebrew")},
		"a bare prefix":                   {"/opt/homebrew", "/opt/homebrew"},
	} {
		got := ForPrefix(tt.prefix)
		if got.Prefix != tt.prefix || got.Cellar != tt.prefix+"/Cellar" || got.Repository != tt.repo || got.Library != tt.repo+"/Library" {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestOpenJDK(t *testing.T) {
	for _, tt := range []struct {
		deps []string
		want string
	}{
		{nil, ""},
		{[]string{"ca-certificates", "oniguruma"}, ""},
		{[]string{"openjdk"}, "openjdk"},
		{[]string{"x", "openjdk@21"}, "openjdk@21"},
		{[]string{"openjdk@17.0"}, "openjdk@17.0"},
		{[]string{"openjdk-extras", "openjdkfoo", "myopenjdk"}, ""},
	} {
		if got := OpenJDK(tt.deps); got != tt.want {
			t.Errorf("OpenJDK(%v) = %q, want %q", tt.deps, got, tt.want)
		}
	}
}
