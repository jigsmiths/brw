// Package guard holds repo-wide checks that run as tests.
package guard

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
)

var banned = map[string]bool{
	"WriteFile": true, "Remove": true, "RemoveAll": true, "Symlink": true,
	"Rename": true, "Mkdir": true, "MkdirAll": true, "MkdirTemp": true,
	"Create": true, "CreateTemp": true, "OpenFile": true, "OpenRoot": true,
	"Link": true, "Chmod": true, "Truncate": true,
}

// findViolations lists banned os calls in src, in source order. A call is
// allowed by an //fsx:allow comment on its line, or alone on the line before.
func findViolations(filename, src string) ([]string, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, src, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	osName := ""
	for _, imp := range f.Imports {
		path, _ := strconv.Unquote(imp.Path.Value)
		if path != "os" {
			continue
		}
		osName = "os"
		if imp.Name != nil {
			osName = imp.Name.Name
		}
	}
	if osName == "" || osName == "_" || osName == "." {
		return nil, nil
	}

	lines := strings.Split(src, "\n")
	allowed := map[int]bool{}
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if !strings.Contains(c.Text, "fsx:allow") {
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
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok || id.Name != osName || !banned[sel.Sel.Name] {
			return true
		}
		if !allowed[fset.Position(sel.Pos()).Line] {
			out = append(out, "os."+sel.Sel.Name)
		}
		return true
	})
	return out, nil
}
