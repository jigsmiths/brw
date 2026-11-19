package cask

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// entry is a file for a test archive; a name ending in / is a directory.
type entry struct {
	name, body string
	mode       os.FileMode
}

func zipBytes(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := zip.NewWriter(&buf)
	for _, e := range entries {
		h := &zip.FileHeader{Name: e.name, Method: zip.Deflate}
		h.SetMode(e.mode | 0o644)
		if strings.HasSuffix(e.name, "/") {
			h.SetMode(fs.ModeDir | 0o755)
		}
		f, err := w.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(e.body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func tarGzBytes(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	w := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: int64(e.mode | 0o644), Size: int64(len(e.body)), Typeflag: tar.TypeReg}
		if strings.HasSuffix(e.name, "/") {
			h = &tar.Header{Name: e.name, Mode: 0o755, Typeflag: tar.TypeDir}
		}
		if err := w.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(e.body))
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func writeFile(t *testing.T, path string, b []byte, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// download puts b in a cache folder under the name Fetch would give it.
func download(t *testing.T, name string, b []byte) string {
	t.Helper()
	return writeFile(t, filepath.Join(t.TempDir(), sum([]byte(name))+"--"+name), b, 0o644)
}

func extract(t *testing.T, file string) string {
	t.Helper()
	dest := filepath.Join(t.TempDir(), "app", "1.0")
	if err := os.MkdirAll(dest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Extract(context.Background(), file, dest); err != nil {
		t.Fatal(err)
	}
	return dest
}

// tree lists the paths under dir, directories with a trailing /.
func tree(t *testing.T, dir string) []string {
	t.Helper()
	var got []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if d.IsDir() {
			rel += "/"
		}
		got = append(got, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func wantTree(t *testing.T, dir string, want ...string) {
	t.Helper()
	if got := tree(t, dir); !slices.Equal(got, want) {
		t.Errorf("tree = %q, want %q", got, want)
	}
}

func needTool(t *testing.T, tool string) {
	t.Helper()
	if _, err := exec.LookPath(tool); err != nil {
		t.Skipf("%s is not available", tool)
	}
}

func TestExtractZip(t *testing.T) {
	needTool(t, "unzip")
	file := download(t, "App.zip", zipBytes(t,
		entry{name: "App.app/"},
		entry{name: "App.app/Contents/MacOS/app", body: "bin", mode: 0o755}))
	dest := extract(t, file)
	wantTree(t, dest, "App.app/", "App.app/Contents/", "App.app/Contents/MacOS/", "App.app/Contents/MacOS/app")
	if fi, err := os.Stat(filepath.Join(dest, "App.app/Contents/MacOS/app")); err != nil || fi.Mode().Perm()&0o100 == 0 {
		t.Errorf("app binary lost its mode: %v %v", fi, err)
	}
	if entries, _ := os.ReadDir(filepath.Dir(dest)); len(entries) != 1 {
		t.Errorf("temporary folders left next to the version: %v", entries)
	}
}

func TestExtractZipWithAppleDoubleFiles(t *testing.T) {
	needTool(t, "ditto")
	src := t.TempDir()
	info := writeFile(t, filepath.Join(src, "App.app", "Info"), []byte("x"), 0o644)
	if err := exec.Command("/usr/bin/xattr", "-w", "com.example.kept", "yes", info).Run(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "App.zip")
	if out, err := exec.Command("ditto", "-c", "-k", "--sequesterRsrc", "--keepParent", filepath.Join(src, "App.app"), file).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if !hasAppleDouble(file) {
		t.Fatal("ditto made a zip without AppleDouble files")
	}
	dest := extract(t, file)
	wantTree(t, dest, "App.app/", "App.app/Info")
	if out, err := exec.Command("/usr/bin/xattr", "-p", "com.example.kept", filepath.Join(dest, "App.app", "Info")).Output(); err != nil || strings.TrimSpace(string(out)) != "yes" {
		t.Errorf("extended attribute lost: %q %v", out, err)
	}
}

func TestExtractTarGz(t *testing.T) {
	file := download(t, "tool.tar.gz", tarGzBytes(t, entry{name: "tool", body: "#!/bin/sh\n", mode: 0o755}, entry{name: "doc/"}))
	wantTree(t, extract(t, file), "doc/", "tool")
}

func TestExtractTarXz(t *testing.T) {
	needTool(t, "xz")
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Vivaldi.app", "Info"), []byte("x"), 0o644)
	file := filepath.Join(t.TempDir(), "v.tar.xz")
	if out, err := exec.Command("tar", "-cJf", file, "-C", src, "Vivaldi.app").CombinedOutput(); err != nil {
		t.Skipf("cannot make a tar.xz: %v %s", err, out)
	}
	wantTree(t, extract(t, file), "Vivaldi.app/", "Vivaldi.app/Info")
}

func TestExtractBareFileKeepsItsName(t *testing.T) {
	file := download(t, "claude", []byte("\xcf\xfa\xed\xfe mach-o"))
	if err := os.Chmod(file, 0o755); err != nil {
		t.Fatal(err)
	}
	dest := extract(t, file)
	wantTree(t, dest, "claude")
	if fi, err := os.Stat(filepath.Join(dest, "claude")); err != nil || fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v, %v", fi, err)
	}
}

func TestExtractUnpacksANestedArchive(t *testing.T) {
	needTool(t, "unzip")
	inner := tarGzBytes(t, entry{name: "App.app/Info", body: "x"})
	file := download(t, "outer.zip", zipBytes(t, entry{name: "inner.tar.gz", body: string(inner)}))
	wantTree(t, extract(t, file), "App.app/", "App.app/Info")
}

func TestExtractKeepsASingleFileThatIsNotAnArchive(t *testing.T) {
	needTool(t, "unzip")
	file := download(t, "tool.zip", zipBytes(t, entry{name: "tool", body: "#!/bin/sh\n", mode: 0o755}))
	wantTree(t, extract(t, file), "tool")
}

func TestExtractRefusesABareGzip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, _ = gz.Write([]byte("not a tar"))
	_ = gz.Close()
	file := download(t, "tool.gz", buf.Bytes())
	dest := t.TempDir()
	if err := Extract(context.Background(), file, dest); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestExtractDmg(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("disk images need macOS")
	}
	src := t.TempDir()
	writeFile(t, filepath.Join(src, "Slack.app", "Contents", "Info.plist"), []byte("<plist/>"), 0o444)
	writeFile(t, filepath.Join(src, ".DS_Store"), []byte("x"), 0o644)
	writeFile(t, filepath.Join(src, ".background", "bg.png"), []byte("x"), 0o644)
	writeFile(t, filepath.Join(src, "README"), []byte("read me"), 0o644)
	if err := os.Symlink("/Applications", filepath.Join(src, "Applications")); err != nil {
		t.Fatal(err)
	}
	// UDBZ images start like a bzip2 file, as slack's and shottr's do.
	for _, format := range []string{"UDZO", "UDBZ"} {
		file := filepath.Join(t.TempDir(), "Slack.dmg")
		if out, err := exec.Command("hdiutil", "create", "-quiet", "-srcfolder", src, "-volname", "Slack", "-format", format, file).CombinedOutput(); err != nil {
			t.Skipf("cannot make a disk image: %v %s", err, out)
		}
		dest := filepath.Join(t.TempDir(), "slack", "1.0")
		if err := os.MkdirAll(dest, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := Extract(context.Background(), file, dest); err != nil {
			t.Errorf("%s: %v", format, err)
			continue
		}
		wantTree(t, dest, "README", "Slack.app/", "Slack.app/Contents/", "Slack.app/Contents/Info.plist")
		if fi, err := os.Stat(filepath.Join(dest, "Slack.app/Contents/Info.plist")); err != nil || fi.Mode().Perm()&0o200 == 0 {
			t.Errorf("%s: files from a disk image should be writable: %v %v", format, fi, err)
		}
		if out, _ := exec.Command("hdiutil", "info").Output(); strings.Contains(string(out), file) {
			t.Errorf("%s: the disk image is still attached:\n%s", format, out)
		}
	}
}
