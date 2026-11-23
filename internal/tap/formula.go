package tap

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/rb"
)

// formulaDir is Tap#formula_dir: Formula, then HomebrewFormula, then the tap itself.
func (t Tap) formulaDir() string {
	for _, d := range []string{"Formula", "HomebrewFormula"} {
		if fi, err := os.Stat(filepath.Join(t.Dir, d)); err == nil && fi.IsDir() {
			return filepath.Join(t.Dir, d)
		}
	}
	return t.Dir
}

// FormulaFiles maps the tap's formula names to their files. Formula and
// HomebrewFormula may be sharded into folders; at the tap's root only the
// top level counts, so casks and commands are not formulae.
func (t Tap) FormulaFiles() (map[string]string, error) {
	dir := t.formulaDir()
	out := map[string]string{}
	if dir == t.Dir {
		files, err := filepath.Glob(filepath.Join(dir, "*.rb"))
		for _, f := range files {
			out[strings.TrimSuffix(filepath.Base(f), ".rb")] = f
		}
		return out, err
	}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".rb") {
			out[strings.TrimSuffix(d.Name(), ".rb")] = p
		}
		return nil
	})
	return out, err
}

func (t Tap) aliases() map[string][]string {
	out := map[string][]string{}
	entries, _ := os.ReadDir(filepath.Join(t.Dir, "Aliases"))
	for _, e := range entries {
		p := filepath.Join(t.Dir, "Aliases", e.Name())
		target, err := filepath.EvalSymlinks(p)
		if err != nil {
			continue
		}
		if fi, err := os.Stat(target); err != nil || !fi.Mode().IsRegular() {
			continue
		}
		name := strings.TrimSuffix(filepath.Base(target), ".rb")
		out[name] = append(out[name], e.Name())
	}
	for _, a := range out {
		slices.Sort(a)
	}
	return out
}

// HasFormula says whether the tap has a formula file for name or an alias
// or old name of one.
func (t Tap) HasFormula(name string) bool {
	files, _ := t.FormulaFiles()
	if _, ok := files[name]; ok {
		return true
	}
	alt, _ := t.AltNames()
	_, ok := alt[name]
	return ok
}

func (t Tap) HasCask(token string) bool {
	files, _ := t.caskFiles()
	return slices.ContainsFunc(files, func(f string) bool {
		return strings.EqualFold(strings.TrimSuffix(filepath.Base(f), ".rb"), token)
	})
}

// AltNames maps the tap's aliases and old names, from its Aliases folder and
// formula_renames.json, to the formula names they stand for.
func (t Tap) AltNames() (map[string]string, error) {
	out := map[string]string{}
	for name, aliases := range t.aliases() {
		for _, a := range aliases {
			out[a] = name
		}
	}
	renames, err := readJSONMap(filepath.Join(t.Dir, "formula_renames.json"))
	maps.Copy(out, renames)
	return out, err
}

func readJSONMap(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m map[string]string
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// ReverseMigrations is brew's Tap.reverse_tap_migrations_renames: tap/name to
// the names it had before a tap_migrations.json moved it.
func ReverseMigrations(taps []Tap) (map[string][]string, error) {
	out := map[string][]string{}
	for _, t := range taps {
		m, err := readJSONMap(filepath.Join(t.Dir, "tap_migrations.json"))
		if err != nil {
			return nil, err
		}
		for _, old := range slices.Sorted(maps.Keys(m)) {
			to := m[old]
			key := to + "/" + old
			if strings.Count(to, "/") == 2 {
				key = to
			}
			out[key] = append(out[key], old)
		}
	}
	return out, nil
}

// setKeys sets top-level keys of a JSON object where they are, so the object
// keeps the order brew writes it in. A key it lacks goes after the key named
// third, else at the end.
func setKeys(obj []byte, set [][3]any) (json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(obj))
	if tok, err := dec.Token(); err != nil {
		return nil, err
	} else if tok != json.Delim('{') {
		return nil, errors.New("not a JSON object")
	}
	var keys []string
	vals := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		keys = append(keys, tok.(string))
		vals[tok.(string)] = v
	}
	for _, kv := range set {
		k := kv[0].(string)
		v, err := json.Marshal(kv[1])
		if err != nil {
			return nil, err
		}
		if _, ok := vals[k]; !ok {
			after, _ := kv[2].(string)
			if i := slices.Index(keys, after); i >= 0 {
				keys = slices.Insert(keys, i+1, k)
			} else {
				keys = append(keys, k)
			}
		}
		vals[k] = v
	}
	var out bytes.Buffer
	out.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		kb, _ := json.Marshal(k)
		out.Write(kb)
		out.WriteByte(':')
		out.Write(vals[k])
	}
	out.WriteByte('}')
	return out.Bytes(), nil
}

// Formula evaluates the tap's formula called name as brew loads it on the
// platform tag, and adds what the tap decides: its full name, aliases, old
// names and where the file came from. migrations is ReverseMigrations of
// every tap. raw is the formula's hash, as Homebrew's API would have it.
func (t Tap) Formula(name, tag string, migrations map[string][]string) (f api.Formula, raw json.RawMessage, err error) {
	files, err := t.FormulaFiles()
	if err != nil {
		return f, nil, err
	}
	file, ok := files[name]
	if !ok {
		return f, nil, fmt.Errorf("%s has no formula %s: %w", t.Name, name, fs.ErrNotExist)
	}
	src, err := os.ReadFile(file)
	if err != nil {
		return f, nil, err
	}
	loaded, err := rb.LoadFormula(src, name, tag)
	if err != nil {
		return f, nil, fmt.Errorf("%s/%s: %w", t.Name, name, err)
	}
	b, err := json.Marshal(loaded)
	if err != nil {
		return f, nil, err
	}
	renames, err := readJSONMap(filepath.Join(t.Dir, "formula_renames.json"))
	if err != nil {
		return f, nil, err
	}
	oldnames := []string{}
	for _, old := range migrations[t.Name+"/"+name] {
		if old != name {
			oldnames = append(oldnames, old)
		}
	}
	for _, old := range slices.Sorted(maps.Keys(renames)) {
		if renames[old] == name && old != name {
			oldnames = append(oldnames, old)
		}
	}
	aliases := t.aliases()[name]
	if aliases == nil {
		aliases = []string{}
	}
	head, _ := run(context.Background(), t.Dir, "rev-parse", "HEAD")
	rel, _ := filepath.Rel(t.Dir, file)
	sum := sha256.Sum256(src)
	raw, err = setKeys(b, [][3]any{
		{"full_name", t.Name + "/" + name, "name"},
		{"tap", t.Name, "full_name"},
		{"oldnames", oldnames, "tap"},
		{"aliases", aliases, "oldnames"},
		// brew finds no versioned formulae for a third-party tap's formula: it
		// keys them by full name and looks them up by name.
		{"versioned_formulae", []string{}, "aliases"},
		{"tap_git_head", head, "service"},
		{"ruby_source_path", filepath.ToSlash(rel), "tap_git_head"},
		{"ruby_source_checksum", map[string]string{"sha256": hex.EncodeToString(sum[:])}, "ruby_source_path"},
	})
	if err != nil {
		return f, nil, err
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		return f, nil, err
	}
	return f, raw, nil
}
