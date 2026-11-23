package tap

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jigsmiths/brw/internal/rb"
)

func formulaFile(name, version string) string {
	return "class " + name + " < Formula\n  desc \"A tool\"\n  url \"https://example.com/tool-" + version + ".tar.gz\"\n  sha256 \"" +
		"0000000000000000000000000000000000000000000000000000000000000000\"\nend\n"
}

func TestFormulaFilesFollowBrewsFormulaDir(t *testing.T) {
	for name, layout := range map[string]struct {
		files []string
		want  []string
	}{
		"Formula":         {[]string{"Formula/a.rb", "Formula/sub/b.rb", "c.rb"}, []string{"a", "b"}},
		"HomebrewFormula": {[]string{"HomebrewFormula/a.rb", "c.rb"}, []string{"a"}},
		// at the root only the top level counts, so Casks are not formulae
		"root": {[]string{"a.rb", "Casks/x.rb", "cmd/y.rb"}, []string{"a"}},
	} {
		dir := t.TempDir()
		for _, f := range layout.files {
			write(t, filepath.Join(dir, f), formulaFile("A", "1.0"))
		}
		files, err := Tap{Name: "u/t", Dir: dir}.FormulaFiles()
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for n := range files {
			got = append(got, n)
		}
		if len(got) != len(layout.want) {
			t.Errorf("%s: got %v, want %v", name, got, layout.want)
		}
		for _, w := range layout.want {
			if _, ok := files[w]; !ok {
				t.Errorf("%s: no %s in %v", name, w, got)
			}
		}
	}
}

func TestFormulaAddsWhatTheTapDecides(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	src := formulaFile("Tool", "1.2")
	write(t, filepath.Join(dir, "Formula", "tool.rb"), src)
	write(t, filepath.Join(dir, "Formula", "tool@1.rb"), formulaFile("ToolAT1", "1.0"))
	if err := os.MkdirAll(filepath.Join(dir, "Aliases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../Formula/tool.rb", filepath.Join(dir, "Aliases", "tool2")); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "formula_renames.json"), `{"oldtool": "tool"}`)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "tool")
	head := git(t, dir, "rev-parse", "HEAD")

	f, raw, err := Tap{Name: "u/t", Dir: dir}.Formula("tool", "arm64_golden_gate", nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.Name != "tool" || f.FullName != "u/t/tool" || f.Tap != "u/t" || f.Versions.Stable != "1.2" {
		t.Errorf("formula: %+v", f)
	}
	if !reflect.DeepEqual(f.Aliases, []string{"tool2"}) || !reflect.DeepEqual(f.OldNames, []string{"oldtool"}) {
		t.Errorf("aliases %v, oldnames %v", f.Aliases, f.OldNames)
	}
	// brew lists no versioned formulae for a tap's formulae, so neither does brw
	if len(f.VersionedFormulae) != 0 {
		t.Errorf("versioned formulae %v", f.VersionedFormulae)
	}
	var h map[string]any
	if err := json.Unmarshal(raw, &h); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(src))
	if h["tap_git_head"] != head[:len(head)-1] || h["ruby_source_path"] != "Formula/tool.rb" ||
		h["ruby_source_checksum"].(map[string]any)["sha256"] != hex.EncodeToString(sum[:]) {
		t.Errorf("source: %v %v %v", h["tap_git_head"], h["ruby_source_path"], h["ruby_source_checksum"])
	}
}

func TestFormulaMigratedFromAnotherTapKeepsItsOldName(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Formula", "tool.rb"), formulaFile("Tool", "1.2"))
	migrations := map[string][]string{"u/t/tool": {"oldtool"}}
	f, _, err := Tap{Name: "u/t", Dir: dir}.Formula("tool", "arm64_golden_gate", migrations)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.OldNames, []string{"oldtool"}) {
		t.Errorf("oldnames %v", f.OldNames)
	}
}

func TestReverseMigrations(t *testing.T) {
	old := t.TempDir()
	write(t, filepath.Join(old, "tap_migrations.json"), `{"tool": "u/t", "other": "u/t/newname", "skip": "homebrew/cask"}`)
	got, err := ReverseMigrations([]Tap{{Name: "o/old", Dir: old}, {Name: "u/t", Dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{"u/t/tool": {"tool"}, "u/t/newname": {"other"}, "homebrew/cask/skip": {"skip"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestFormulaSaysWhyItCannot(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "Formula", "bad.rb"), "require_relative \"../lib/x\"\nclass Bad < Formula\nend\n")
	tp := Tap{Name: "u/t", Dir: dir}
	if _, _, err := tp.Formula("bad", "arm64_golden_gate", nil); !errors.Is(err, rb.ErrUnsupported) {
		t.Errorf("bad: %v, want ErrUnsupported", err)
	}
	if _, _, err := tp.Formula("missing", "arm64_golden_gate", nil); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: %v, want ErrNotExist", err)
	}
}
