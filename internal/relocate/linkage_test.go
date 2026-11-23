package relocate

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/machoedit"
)

const typeBundle = 8

func TestFixLinkageIsBrewsFixDynamicLinkage(t *testing.T) {
	prefix := t.TempDir()
	r, err := fsx.Open(prefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	const built = "Cellar/tool/1.0"
	kegAbs := filepath.Join(prefix, built)
	// not under /tmp, where the prefix itself is on Linux
	build := "/brw-build/tool-20261123-1/tool-1.0"
	files := map[string]string{
		// A library linked in the build directory gets its opt ID and a
		// @loader_path name for its neighbour.
		"lib/libtool.1.dylib": macho(typeDylib,
			dylib(cmdID, build+"/.libs/libtool.1.dylib"),
			dylib(cmdLoad, "libhelper.dylib"),
			dylib(cmdLoad, "/usr/lib/libSystem.B.dylib"),
			dylib(cmdLoad, "@rpath/libkept.dylib"),
			rpath(prefix+"/Cellar/dep/2.0/lib"),
			rpath(build+"/.libs"),
			rpath("/usr/local/lib"),
			rpath("/usr/local/lib"),
		),
		"lib/libhelper.dylib":   macho(typeDylib, dylib(cmdID, "@rpath/libhelper.dylib")),
		"lib/sub/libdeep.dylib": macho(typeDylib, dylib(cmdID, "/elsewhere/libdeep.dylib")),
		// A program finds its library in the keg's lib, or by searching it.
		"bin/tool": macho(typeExecute,
			dylib(cmdLoad, "libtool.1.dylib"),
			dylib(cmdLoad, build+"/sub/.libs/libdeep.dylib"),
			dylib(cmdLoad, "/brw-build/tool-20261123-1/missing.dylib"),
			dylib(cmdLoad, prefix+"/opt/dep/lib/libdep.dylib"),
		),
		"libexec/plugin.so": macho(typeBundle, dylib(cmdLoad, "/usr/lib/libSystem.B.dylib")),
		"share/readme":      "not a binary",
	}
	for name, body := range files {
		if err := r.WriteFile(filepath.Join(built, name), []byte(body), 0o444); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.Link(filepath.Join(built, "bin/tool"), filepath.Join(built, "bin/tool-link")); err != nil {
		t.Fatal(err)
	}
	var warnings []string
	modified, err := FixLinkage(r, built, Linkage{
		Prefix: prefix, BuildDirs: []string{"/brw-build"},
		Warn: func(s string) { warnings = append(warnings, s) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"bin/tool", "lib/libhelper.dylib", "lib/libtool.1.dylib", "lib/sub/libdeep.dylib"}; !reflect.DeepEqual(modified, want) {
		t.Errorf("modified = %v, want %v", modified, want)
	}
	parse := func(name string) *machoedit.File {
		b, err := os.ReadFile(filepath.Join(kegAbs, name))
		if err != nil {
			t.Fatal(err)
		}
		f, err := machoedit.Parse(b)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	opt := prefix + "/opt/tool"
	lib := parse("lib/libtool.1.dylib")
	if id, _ := lib.DylibID(); id != opt+"/lib/libtool.1.dylib" {
		t.Errorf("id = %q", id)
	}
	if got, want := lib.Dylibs(), []string{"@loader_path/libhelper.dylib", "/usr/lib/libSystem.B.dylib", "@rpath/libkept.dylib"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dylibs = %v, want %v", got, want)
	}
	if got, want := lib.Rpaths(), []string{prefix + "/opt/dep/lib", "/usr/local/lib"}; !reflect.DeepEqual(got, want) {
		t.Errorf("rpaths = %v, want %v (opt, without the build directory or the repeat)", got, want)
	}
	if id, _ := parse("lib/sub/libdeep.dylib").DylibID(); id != opt+"/lib/sub/libdeep.dylib" {
		t.Errorf("deep id = %q", id)
	}
	if id, _ := parse("lib/libhelper.dylib").DylibID(); id != opt+"/lib/libhelper.dylib" {
		t.Errorf("helper id = %q", id)
	}
	want := []string{kegAbs + "/lib/libtool.1.dylib", kegAbs + "/lib/sub/libdeep.dylib",
		"/brw-build/tool-20261123-1/missing.dylib", prefix + "/opt/dep/lib/libdep.dylib"}
	for _, name := range []string{"bin/tool", "bin/tool-link"} {
		if got := parse(name).Dylibs(); !reflect.DeepEqual(got, want) {
			t.Errorf("%s dylibs = %v\nwant %v", name, got, want)
		}
	}
	if fi, _ := os.Stat(filepath.Join(kegAbs, "bin/tool")); fi.Mode().Perm() != 0o444 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if want := []string{"Could not fix /brw-build/tool-20261123-1/missing.dylib in " + kegAbs + "/bin/tool"}; !reflect.DeepEqual(warnings, want) {
		t.Errorf("warnings = %q", warnings)
	}
}

// CMake writes /tmp for /private/tmp, so brew counts both as the build's.
func TestInBuildDirCoversTmpForPrivateTmp(t *testing.T) {
	x := &fixer{l: Linkage{BuildDirs: []string{"/private/tmp"}}}
	for name, want := range map[string]bool{
		"/private/tmp/tool-1/lib/a.dylib": true, "/tmp/tool-1/lib/a.dylib": true,
		"/private/tmpfoo/a.dylib": false, "/usr/local/lib/a.dylib": false,
	} {
		if got := x.inBuildDir(name); got != want {
			t.Errorf("inBuildDir(%s) = %v, want %v", name, got, want)
		}
	}
}
