package cellar

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestReceiptRoundTrip(t *testing.T) {
	// Covers optional keys: built_prefix, installed_as_dependency,
	// linkage_files and a missing loaded_from_internal_api.
	for _, name := range []string{"jq", "bash", "brotli", "age"} {
		t.Run(name, func(t *testing.T) {
			orig := fixture(t, name)
			r, err := ParseReceipt(orig)
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			var want, got any
			if err := json.Unmarshal(orig, &want); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(out, &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(want, got) {
				t.Errorf("round trip changed the receipt\nwant %s\ngot  %s", orig, out)
			}
		})
	}
}

func TestReceiptBytesIdentical(t *testing.T) {
	for _, name := range []string{"jq", "bash", "brotli", "age"} {
		t.Run(name, func(t *testing.T) {
			orig := fixture(t, name)
			r, err := ParseReceipt(orig)
			if err != nil {
				t.Fatal(err)
			}
			out, err := r.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if string(out) != string(orig) {
				t.Errorf("bytes differ\nwant %q\ngot  %q", orig, out)
			}
		})
	}
}

func TestReceiptFields(t *testing.T) {
	r, err := ParseReceipt(fixture(t, "jq"))
	if err != nil {
		t.Fatal(err)
	}
	if !r.InstalledOnRequest || !r.PouredFromBottle {
		t.Errorf("flags = on_request %v, poured %v", r.InstalledOnRequest, r.PouredFromBottle)
	}
	if r.Arch != "arm64" {
		t.Errorf("Arch = %q", r.Arch)
	}
	if len(r.RuntimeDependencies) != 1 || r.RuntimeDependencies[0].FullName != "oniguruma" ||
		!r.RuntimeDependencies[0].DeclaredDirectly {
		t.Errorf("RuntimeDependencies = %+v", r.RuntimeDependencies)
	}
	if r.Source.Tap != "homebrew/core" {
		t.Errorf("Source.Tap = %q", r.Source.Tap)
	}

	dep, err := ParseReceipt(fixture(t, "brotli"))
	if err != nil {
		t.Fatal(err)
	}
	if dep.InstalledOnRequest || dep.InstalledAsDependency == nil || !*dep.InstalledAsDependency {
		t.Errorf("brotli flags = on_request %v, as_dependency %v", dep.InstalledOnRequest, dep.InstalledAsDependency)
	}
}

func TestParseReceiptInvalid(t *testing.T) {
	if _, err := ParseReceipt([]byte("{not json")); err == nil {
		t.Error("expected error")
	}
}

func TestKegs(t *testing.T) {
	root := t.TempDir()
	p := Prefix(root)
	writeFile(t, filepath.Join(root, "Cellar/jq/1.8.2/INSTALL_RECEIPT.json"), string(fixture(t, "jq")))
	writeFile(t, filepath.Join(root, "Cellar/multi/1.0/INSTALL_RECEIPT.json"), string(fixture(t, "age")))
	writeFile(t, filepath.Join(root, "Cellar/multi/2.0/INSTALL_RECEIPT.json"), string(fixture(t, "age")))
	writeFile(t, filepath.Join(root, "Cellar/noreceipt/0.1/bin/x"), "")
	if err := os.MkdirAll(filepath.Join(root, "Cellar/empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "Cellar/.DS_Store"), "")

	kegs, err := p.Kegs()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, k := range kegs {
		got = append(got, k.Name+"@"+k.Version)
	}
	want := []string{"jq@1.8.2", "multi@1.0", "multi@2.0", "noreceipt@0.1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Kegs = %v, want %v", got, want)
	}
	if kegs[0].Receipt == nil || kegs[0].Receipt.Arch != "arm64" {
		t.Errorf("jq receipt = %+v", kegs[0].Receipt)
	}
	if kegs[3].Receipt != nil {
		t.Errorf("noreceipt should have a nil receipt")
	}
	if kegs[0].Path != filepath.Join(root, "Cellar/jq/1.8.2") {
		t.Errorf("Path = %q", kegs[0].Path)
	}
}

func TestKegsNoCellar(t *testing.T) {
	kegs, err := Prefix(t.TempDir()).Kegs()
	if err != nil || len(kegs) != 0 {
		t.Errorf("Kegs = %v, %v; want none", kegs, err)
	}
}

func TestKegsBadReceipt(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "Cellar/bad/1.0/INSTALL_RECEIPT.json"), "{oops")
	if _, err := Prefix(root).Kegs(); err == nil {
		t.Error("expected error for unparseable receipt")
	}
}

func TestOptVersion(t *testing.T) {
	root := t.TempDir()
	p := Prefix(root)
	writeFile(t, filepath.Join(root, "Cellar/jq/1.8.2/bin/jq"), "")
	symlink(t, "../Cellar/jq/1.8.2", filepath.Join(root, "opt/jq"))
	symlink(t, "../Cellar/gone/1.0", filepath.Join(root, "opt/gone"))

	if v, ok := p.OptVersion("jq"); !ok || v != "1.8.2" {
		t.Errorf("OptVersion(jq) = %q, %v", v, ok)
	}
	if _, ok := p.OptVersion("gone"); ok {
		t.Error("dangling opt link should not count")
	}
	if _, ok := p.OptVersion("missing"); ok {
		t.Error("missing opt link should not count")
	}
}

func TestLinked(t *testing.T) {
	root := t.TempDir()
	p := Prefix(root)
	writeFile(t, filepath.Join(root, "Cellar/jq/1.8.2/bin/jq"), "")
	writeFile(t, filepath.Join(root, "Cellar/jq/1.8.1/bin/jq"), "")
	symlink(t, "../../../Cellar/jq/1.8.2", filepath.Join(root, "var/homebrew/linked/jq"))
	symlink(t, "../../../Cellar/gone/1.0", filepath.Join(root, "var/homebrew/linked/gone"))
	writeFile(t, filepath.Join(root, "var/homebrew/linked/plain"), "not a symlink")

	tests := []struct {
		name, version string
		want          bool
	}{
		{"jq", "1.8.2", true},
		{"jq", "1.8.1", false}, // linked record points at another version
		{"gone", "1.0", false},
		{"plain", "1.0", false},
		{"missing", "1.0", false},
	}
	for _, tt := range tests {
		if got := p.Linked(tt.name, tt.version); got != tt.want {
			t.Errorf("Linked(%s, %s) = %v, want %v", tt.name, tt.version, got, tt.want)
		}
	}
}

func TestReceiptUnknownKeys(t *testing.T) {
	mutations := map[string]func(m map[string]any){
		"top-level key": func(m map[string]any) { m["future_key"] = true },
		"dependency key": func(m map[string]any) {
			deps := m["runtime_dependencies"].([]any)
			deps[0].(map[string]any)["future_dep_key"] = 1
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal(fixture(t, "jq"), &m); err != nil {
				t.Fatal(err)
			}
			mutate(m)
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			r, err := ParseReceipt(b)
			if err != nil {
				t.Fatalf("reading must tolerate unknown keys: %v", err)
			}
			if _, err := r.Marshal(); !errors.Is(err, ErrUnknownKeys) {
				t.Errorf("Marshal error = %v, want ErrUnknownKeys", err)
			}
		})
	}
}
