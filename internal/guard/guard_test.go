package guard

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindViolations(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []string
	}{
		{"clean read-only", `package p
import "os"
func f() { os.ReadFile("x"); os.Stat("x"); os.Open("x"); os.ReadDir("x"); os.Readlink("x") }`, nil},
		{"write", `package p
import "os"
func f() { os.WriteFile("x", nil, 0o644) }`, []string{"os.WriteFile"}},
		{"several", `package p
import "os"
func f() { os.Remove("a"); os.RemoveAll("b"); os.Symlink("a", "b"); os.Rename("a", "b"); os.MkdirAll("a", 0o755) }`,
			[]string{"os.Remove", "os.RemoveAll", "os.Symlink", "os.Rename", "os.MkdirAll"}},
		{"create and open for write", `package p
import "os"
func f() { os.Create("x"); os.OpenFile("x", 0, 0); os.OpenRoot("x") }`,
			[]string{"os.Create", "os.OpenFile", "os.OpenRoot"}},
		{"aliased import", `package p
import o "os"
func f() { o.Remove("x") }`, []string{"os.Remove"}},
		{"other package named os", `package p
import os "example.com/notos"
func f() { os.Remove("x") }`, nil},
		{"allow on same line", `package p
import "os"
func f() { os.Remove("x") //fsx:allow cache file
}`, nil},
		{"allow on previous line", `package p
import "os"
func f() {
	//fsx:allow journal file
	os.OpenFile("x", 0, 0)
}`, nil},
		{"allow does not leak to next call", `package p
import "os"
func f() {
	os.Remove("a") //fsx:allow cache
	os.Remove("b")
}`, []string{"os.Remove"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findViolations("x.go", tt.src)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("violations = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNoDirectFilesystemWrites(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "testdata":
				return fs.SkipDir
			case "fsx":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		found, err := findViolations(path, string(src))
		if err != nil {
			return err
		}
		for _, v := range found {
			t.Errorf("%s: %s outside internal/fsx (add //fsx:allow <reason> if it is not a prefix write)", path, v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
