package install

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/fsx"
)

// kegHost spans the prefix and the build directory: each path goes to its
// own root, and moving or linking from one into the other works.
func TestKegHostSpansTheBuildDirectoryAndThePrefix(t *testing.T) {
	prefix, build := t.TempDir(), t.TempDir()
	pr, err := fsx.Open(prefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	br, err := fsx.Open(build, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = pr.Close(), br.Close() })
	h := &kegHost{root: pr, build: br}
	keg := filepath.Join(prefix, "Cellar/tool/1.0")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(h.MkdirAll(filepath.Join(build, "out")))
	must(h.WriteFile(filepath.Join(build, "out/tool"), []byte("t"), 0o755))
	must(h.WriteFile(filepath.Join(build, "out/doc"), []byte("d"), 0o644))
	must(h.MkdirAll(filepath.Join(keg, "bin")))
	must(h.Rename(filepath.Join(build, "out/tool"), filepath.Join(keg, "bin/tool")))
	must(h.Rename(filepath.Join(build, "out/doc"), filepath.Join(build, "doc")))
	must(h.Symlink(filepath.Join(keg, "bin/tool"), filepath.Join(build, "link")))
	must(h.Chmod(filepath.Join(keg, "bin/tool"), 0o555))
	if b, err := h.ReadFile(filepath.Join(keg, "bin/tool")); err != nil || string(b) != "t" {
		t.Errorf("moved file: %q, %v", b, err)
	}
	if _, err := h.Lstat(filepath.Join(build, "doc")); err != nil {
		t.Errorf("renamed within the build directory: %v", err)
	}
	if l, _ := os.Readlink(filepath.Join(build, "link")); l != filepath.Join(keg, "bin/tool") {
		t.Errorf("link -> %q", l)
	}
	if m, err := h.Glob(filepath.Join(build, "*")); err != nil || len(m) != 3 {
		t.Errorf("glob in the build directory = %v, %v", m, err)
	}
	if _, err := h.Glob("/etc/*"); err == nil {
		t.Error("glob outside both roots should fail")
	}
	if entries, _ := h.ReadDir(filepath.Join(build, "out")); len(entries) != 0 {
		t.Errorf("left in out: %v", entries)
	}
	must(h.Remove(filepath.Join(build, "link")))
	must(h.RemoveAll(filepath.Join(build, "out")))
	if _, err := os.Lstat(filepath.Join(build, "out")); err == nil {
		t.Error("RemoveAll left out behind")
	}
}

// unpack extracts zips and tarballs, and copies anything else in under its
// name with the download's time, as brew's Uncompressed strategy does.
func TestUnpackLikeBrewsStrategies(t *testing.T) {
	dl := t.TempDir()
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, err := zw.Create("tool-1.0/tool")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("zipped")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	zipFile := filepath.Join(dl, "tool--1.0--tool.zip")
	plain := filepath.Join(dl, "tool--1.0--tool")
	mtime := time.Unix(1791806400, 0)
	for name, data := range map[string][]byte{zipFile: zbuf.Bytes(), plain: []byte("#!/bin/sh\n")} {
		if err := os.WriteFile(name, data, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	root, err := fsx.Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	in := &Installer{}
	for _, d := range []string{"z", "p"} {
		if err := root.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := in.unpack(root, zipFile, "tool.zip", filepath.Join(dir, "z")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "z/tool-1.0/tool")); string(b) != "zipped" {
		t.Errorf("zip: %q", b)
	}
	if bp, _ := buildpath(filepath.Join(dir, "z")); bp != filepath.Join(dir, "z/tool-1.0") {
		t.Errorf("buildpath = %s", bp)
	}
	if err := in.unpack(root, plain, "tool", filepath.Join(dir, "p")); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "p/tool"))
	if err != nil || !fi.ModTime().Equal(mtime) || fi.Mode().Perm() != 0o755 {
		t.Errorf("plain file: %v %v", fi, err)
	}
	if err := in.unpack(root, filepath.Join(dl, "missing.tar.gz"), "missing.tar.gz", filepath.Join(dir, "p")); err == nil {
		t.Error("unpacking a missing archive should fail")
	}
}

// tarCommit is Utils::Git.get_tar_commit_id: the commit git archive, and so
// GitHub, writes into a tarball's header.
func TestTarCommitIsGitsGetTarCommitID(t *testing.T) {
	const sha = "4cf22279b1fb4cf22279b1fb4cf22279b1fb4cf2"
	dir := t.TempDir()
	write := func(name string, gz, withComment bool) string {
		var buf bytes.Buffer
		var w io.Writer = &buf
		var zw *gzip.Writer
		if gz {
			zw = gzip.NewWriter(&buf)
			w = zw
		}
		tw := tar.NewWriter(w)
		if withComment {
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeXGlobalHeader, Name: "pax_global_header",
				PAXRecords: map[string]string{"comment": sha}, Format: tar.FormatPAX}); err != nil {
				t.Fatal(err)
			}
		}
		body := bytes.Repeat([]byte("x"), 2048)
		if err := tw.WriteHeader(&tar.Header{Name: "tool-1.0/a", Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		if zw != nil {
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tt := range []struct {
		file string
		want string
	}{
		{write("a.tar.gz", true, true), sha},
		{write("a.tgz", true, true), sha},
		{write("a.tar", false, true), sha},
		{write("b.tar.gz", true, false), ""},
		{write("a.zip", false, true), ""}, // brew reads only tarballs
	} {
		if got := tarCommit(tt.file); got != tt.want {
			t.Errorf("%s: %q, want %q", filepath.Base(tt.file), got, tt.want)
		}
	}
}

func TestStdenvIsBrewsStdenvAndSandbox(t *testing.T) {
	p := t.TempDir()
	params := stdenvParams{Prefix: p, Buildpath: "/b/tool-1.0", Cache: "/c", SDK: "/sdk", MacVersion: "27.2",
		Jobs: 8, DepBins: []string{p + "/opt/go/bin"}, Temp: "/private/tmp",
		Keg: p + "/Cellar/tool/1.0", SourceModified: time.Unix(1791806400, 0)}
	got := stdenv(params)
	sdkFlags := "-Os -w -pipe -mmacosx-version-min=27.2 -isysroot/sdk"
	for k, want := range map[string]string{
		"HOMEBREW_ENV": "std", "PATH": p + "/opt/go/bin:/usr/bin:/bin:/usr/sbin:/sbin", "MAKEFLAGS": "-j8",
		"CC": "/usr/bin/clang", "CXX": "/usr/bin/clang++", "OBJC": "/usr/bin/clang", "OBJCXX": "/usr/bin/clang++",
		"CFLAGS": sdkFlags, "CXXFLAGS": sdkFlags, "OBJCFLAGS": sdkFlags, "OBJCXXFLAGS": sdkFlags,
		"CPPFLAGS": "-isystem" + p + "/include -isysroot/sdk",
		"LDFLAGS":  "-L" + p + "/lib -Wl,-headerpad_max_install_names -isysroot/sdk",
		"CPATH":    p + "/include:/sdk/usr/include", "SDKROOT": "/sdk", "LC_CTYPE": "C",
		"CMAKE_PREFIX_PATH": p + ":/sdk/usr", "CMAKE_FRAMEWORK_PATH": "/sdk/System/Library/Frameworks",
		"HOME": "/b/tool-1.0/.brew_home", "TMPDIR": "/private/tmp", "GOENV": "off", "GIT_TERMINAL_PROMPT": "0",
		"GIT_CONFIG_GLOBAL": "/dev/null", "XDG_CONFIG_HOME": "/b/tool-1.0/.brew_home/.config",
		"GOCACHE": "/c/go_cache", "GOPATH": "/c/go_mod_cache", "CARGO_HOME": "/c/cargo_cache",
		"CARGO_REGISTRY_GLOBAL_MIN_PUBLISH_AGE": "1 day", "PYTHONDONTWRITEBYTECODE": "1",
		// what build.rb sets around the install method
		"HOMEBREW_FORMULA_PREFIX": p + "/Cellar/tool/1.0", "HOMEBREW_FORMULA_BUILDPATH": "/b/tool-1.0",
		"SOURCE_DATE_EPOCH": "1791806400", "TZ": "UTC0",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}

	// A prefix with Frameworks adds them before the SDK, as Stdenv does.
	if err := os.Mkdir(filepath.Join(p, "Frameworks"), 0o755); err != nil {
		t.Fatal(err)
	}
	got = stdenv(params)
	for k, want := range map[string]string{
		"CPPFLAGS":             "-isystem" + p + "/include -F" + p + "/Frameworks -isysroot/sdk",
		"LDFLAGS":              "-L" + p + "/lib -F" + p + "/Frameworks -Wl,-headerpad_max_install_names -isysroot/sdk",
		"CMAKE_FRAMEWORK_PATH": p + "/Frameworks:/sdk/System/Library/Frameworks",
	} {
		if got[k] != want {
			t.Errorf("with Frameworks, %s = %q, want %q", k, got[k], want)
		}
	}
}
