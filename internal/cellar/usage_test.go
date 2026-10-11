package cellar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUsage(t *testing.T) {
	root := t.TempDir()
	keg := filepath.Join(root, "Cellar/pkg/1.0")
	writeFile(t, filepath.Join(keg, "bin/tool"), strings.Repeat("x", 1500))
	writeFile(t, filepath.Join(keg, "share/doc.txt"), "abc")
	writeFile(t, filepath.Join(keg, "share/.DS_Store"), "junk")
	if err := os.Link(filepath.Join(keg, "bin/tool"), filepath.Join(keg, "bin/tool-link")); err != nil {
		t.Fatal(err)
	}
	symlink(t, "bin", filepath.Join(keg, "dirlink"))
	symlink(t, "bin/tool", filepath.Join(keg, "filelink"))

	files, size, err := Keg{Path: keg}.Usage()
	if err != nil {
		t.Fatal(err)
	}
	// tool, tool-link, doc.txt and filelink count; .DS_Store and the directory symlink do not.
	if files != 4 {
		t.Errorf("files = %d, want 4", files)
	}

	// The hardlink is counted once. Directories and symlinks count their own size.
	want := int64(1500 + 3 + 4) // tool, doc.txt, .DS_Store
	for _, p := range []string{"", "bin", "share"} {
		fi, err := os.Lstat(filepath.Join(keg, p))
		if err != nil {
			t.Fatal(err)
		}
		want += fi.Size()
	}
	for _, p := range []string{"dirlink", "filelink"} {
		fi, err := os.Lstat(filepath.Join(keg, p))
		if err != nil {
			t.Fatal(err)
		}
		want += fi.Size()
	}
	if size != want {
		t.Errorf("size = %d, want %d", size, want)
	}
}

func TestUsageDanglingSymlink(t *testing.T) {
	keg := filepath.Join(t.TempDir(), "k")
	symlink(t, "nowhere", filepath.Join(keg, "broken"))
	files, _, err := Keg{Path: keg}.Usage()
	if err != nil || files != 1 {
		t.Errorf("files = %d, %v; a dangling link is one file", files, err)
	}
}

func TestFormatAbv(t *testing.T) {
	tests := []struct {
		files int
		size  int64
		want  string
	}{
		{1735, 66317179, "1,735 files, 66.3MB"}, // git
		{20, 1268791, "20 files, 1.3MB"},        // jq
		{13, 5388005, "13 files, 5.4MB"},        // sqlite
		{1, 512, "512B"},                        // one file is not counted out
		{0, 0, "0B"},
		{2, 999, "2 files, 999B"},
		{2, 1000, "2 files, 1KB"},   // whole numbers drop the decimal
		{2, 999960, "2 files, 1MB"}, // rounds up into the next unit
		{2, 1500, "2 files, 1.5KB"},
		{1234567, 2_500_000_000, "1,234,567 files, 2.5GB"},
	}
	for _, tt := range tests {
		if got := formatAbv(tt.files, tt.size); got != tt.want {
			t.Errorf("formatAbv(%d, %d) = %q, want %q", tt.files, tt.size, got, tt.want)
		}
	}
}

func TestAbv(t *testing.T) {
	keg := filepath.Join(t.TempDir(), "k")
	writeFile(t, filepath.Join(keg, "a"), "x")
	writeFile(t, filepath.Join(keg, "b"), "y")
	got, err := Keg{Path: keg}.Abv()
	if err != nil || !strings.HasPrefix(got, "2 files, ") {
		t.Errorf("Abv = %q, %v", got, err)
	}
}
