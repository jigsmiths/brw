package guard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sentinelAllow = "guard:allow"

var basicTypes = map[string]bool{
	"bool": true, "string": true, "int": true, "int8": true, "int16": true, "int32": true, "int64": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true, "float32": true, "float64": true,
}

// findSentinels lists state encoded in a nil pointer or a stored -1. Pointer
// fields with a json tag pass, since brew's JSON needs null. A line is allowed
// by "//guard:allow <reason>" on it or alone on the line before.
func findSentinels(filename, src string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	lines := strings.Split(src, "\n")
	allowed := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			_, reason, ok := strings.Cut(c.Text, sentinelAllow)
			if !ok || strings.TrimSpace(reason) == "" {
				continue
			}
			pos := fset.Position(c.Pos())
			allowed[pos.Line] = true
			if strings.TrimSpace(lines[pos.Line-1][:pos.Column-1]) == "" {
				allowed[pos.Line+1] = true
			}
		}
	}

	var out []string
	report := func(n ast.Node, msg string) {
		line := fset.Position(n.Pos()).Line
		if !allowed[line] {
			out = append(out, fmt.Sprintf("%d: %s", line, msg))
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.StructType:
			for _, fd := range n.Fields.List {
				star, ok := fd.Type.(*ast.StarExpr)
				if !ok || len(fd.Names) == 0 {
					continue
				}
				id, ok := star.X.(*ast.Ident)
				if !ok || !basicTypes[id.Name] {
					continue
				}
				if fd.Tag != nil && strings.Contains(fd.Tag.Value, `json:"`) {
					continue
				}
				report(fd, fmt.Sprintf("field %s *%s: nil as a state; use an explicit field or an enum", fd.Names[0].Name, id.Name))
			}
		case *ast.AssignStmt:
			for i, rhs := range n.Rhs {
				if i >= len(n.Lhs) || !isMinusOne(rhs) {
					continue
				}
				switch n.Lhs[i].(type) {
				case *ast.IndexExpr, *ast.SelectorExpr:
					report(n, "-1 stored as a state; use a set or an explicit field")
				}
			}
		}
		return true
	})
	return out, nil
}

func isMinusOne(e ast.Expr) bool {
	u, ok := e.(*ast.UnaryExpr)
	if !ok || u.Op != token.SUB {
		return false
	}
	lit, ok := u.X.(*ast.BasicLit)
	return ok && lit.Kind == token.INT && lit.Value == "1"
}

func TestFindSentinels(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want int
	}{
		{"pointer field", `package p
type Item struct{ Linked *bool }`, 1},
		{"pointer field in anonymous struct", `package p
var x struct{ N *int }`, 1},
		{"json pointer field", "package p\ntype R struct{ Head *string `json:\"head\"` }", 0},
		{"pointer param", `package p
func f(dryRun *bool) {}`, 0},
		{"pointer to struct", `package p
type T struct{ R *Receipt }`, 0},
		{"minus one in map", `package p
func f(m map[string]int) { m["a"] = -1 }`, 1},
		{"minus one in field", `package p
func f(w *W) { w.pos = -1 }`, 1},
		{"minus one local", `package p
func f() { n := -1; _ = n }`, 0},
		{"allowed with reason", `package p
func f(m map[string]int) { m["a"] = -1 //guard:allow sorts first
}`, 0},
		{"allow needs a reason", `package p
func f(m map[string]int) { m["a"] = -1 //guard:allow
}`, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := findSentinels("x.go", tt.src)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != tt.want {
				t.Errorf("sentinels = %v, want %d", got, tt.want)
			}
		})
	}
}

func TestNoSentinelState(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" {
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
		found, err := findSentinels(path, string(src))
		if err != nil {
			return err
		}
		for _, v := range found {
			t.Errorf("%s:%s (or add //%s <reason>)", path, v, sentinelAllow)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
