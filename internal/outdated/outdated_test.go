package outdated

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/cellar"
)

type formula struct {
	name     string
	version  string
	revision int
	scheme   int
}

func index(t *testing.T, formulae ...formula) *api.Index {
	t.Helper()
	var list []map[string]any
	for _, f := range formulae {
		list = append(list, map[string]any{
			"name":           f.name,
			"tap":            "homebrew/core",
			"versions":       map[string]any{"stable": f.version},
			"revision":       f.revision,
			"version_scheme": f.scheme,
		})
	}
	b, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	fs, err := api.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return api.NewIndex(fs)
}

type keg struct {
	name, version string
	scheme        int
	tap           string // receipt tap; empty leaves it out
	linked        bool   // opt and linked records point at this keg
	pinned        bool
}

func prefix(t *testing.T, kegs ...keg) cellar.Prefix {
	t.Helper()
	dir := t.TempDir()
	for _, k := range kegs {
		path := filepath.Join(dir, "Cellar", k.name, k.version)
		receipt := fmt.Sprintf(`{"source":{"tap":%q,"versions":{"version_scheme":%d}}}`, k.tap, k.scheme)
		must(t, os.MkdirAll(path, 0o755))
		must(t, os.WriteFile(filepath.Join(path, "INSTALL_RECEIPT.json"), []byte(receipt), 0o644))
		if k.linked {
			link(t, "../Cellar/"+k.name+"/"+k.version, filepath.Join(dir, "opt", k.name))
			link(t, "../../../Cellar/"+k.name+"/"+k.version, filepath.Join(dir, "var/homebrew/linked", k.name))
		}
		if k.pinned {
			link(t, "../../../Cellar/"+k.name+"/"+k.version, filepath.Join(dir, "var/homebrew/pinned", k.name))
		}
	}
	return cellar.Prefix(dir)
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func link(t *testing.T, target, path string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.Symlink(target, path))
}

func check(t *testing.T, idx *api.Index, p cellar.Prefix) []Result {
	t.Helper()
	kegs, err := p.Kegs()
	must(t, err)
	got, err := Check(idx, p, kegs)
	must(t, err)
	return got
}

func names(rs []Result) []string {
	out := []string{}
	for _, r := range rs {
		out = append(out, r.Name)
	}
	return out
}

func TestUpToDate(t *testing.T) {
	got := check(t, index(t, formula{name: "jq", version: "1.8"}), prefix(t, keg{name: "jq", version: "1.8", linked: true}))
	if len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestOlderKegIsOutdated(t *testing.T) {
	got := check(t, index(t, formula{name: "jq", version: "1.9"}), prefix(t, keg{name: "jq", version: "1.8", linked: true}))
	want := []Result{{Name: "jq", InstalledVersions: []string{"1.8"}, CurrentVersion: "1.9"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestRevisionBump(t *testing.T) {
	idx := index(t, formula{name: "jq", version: "1.8", revision: 1})
	old := check(t, idx, prefix(t, keg{name: "jq", version: "1.8", linked: true}))
	if len(old) != 1 || old[0].CurrentVersion != "1.8_1" {
		t.Errorf("1.8 against 1.8_1 = %+v", old)
	}
	current := check(t, idx, prefix(t, keg{name: "jq", version: "1.8_1", linked: true}))
	if len(current) != 0 {
		t.Errorf("1.8_1 against 1.8_1 = %+v", current)
	}
}

func TestKegAheadOfFormulaIsCurrent(t *testing.T) {
	got := check(t, index(t, formula{name: "jq", version: "1.8"}), prefix(t, keg{name: "jq", version: "1.9", linked: true}))
	if len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestUnlinkedCurrentKegIsOutdated(t *testing.T) {
	got := check(t, index(t, formula{name: "jq", version: "1.8"}), prefix(t, keg{name: "jq", version: "1.8"}))
	if len(got) != 1 || got[0].CurrentVersion != "1.8" {
		t.Errorf("got %+v", got)
	}
}

func TestPinnedUnlinkedCurrentKegIsCurrent(t *testing.T) {
	got := check(t, index(t, formula{name: "jq", version: "1.8"}), prefix(t, keg{name: "jq", version: "1.8", pinned: true}))
	if len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestOnlyOldKegLinkedListsAllVersionsSorted(t *testing.T) {
	p := prefix(t,
		keg{name: "jq", version: "1.10"},
		keg{name: "jq", version: "1.8", linked: true},
		keg{name: "jq", version: "1.9"})
	got := check(t, index(t, formula{name: "jq", version: "1.10"}), p)
	want := []string{"1.8", "1.9", "1.10"}
	if len(got) != 1 || !reflect.DeepEqual(got[0].InstalledVersions, want) {
		t.Errorf("got %+v, want versions %v", got, want)
	}
}

func TestLinkedCurrentKegAmongOldOnes(t *testing.T) {
	p := prefix(t, keg{name: "jq", version: "1.7"}, keg{name: "jq", version: "1.8", linked: true})
	if got := check(t, index(t, formula{name: "jq", version: "1.8"}), p); len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestVersionScheme(t *testing.T) {
	// The formula moved to scheme 1, so a scheme 0 keg is outdated even though
	// its version number looks newer.
	idx := index(t, formula{name: "a", version: "1.0", scheme: 1})
	if got := check(t, idx, prefix(t, keg{name: "a", version: "2.0", linked: true})); len(got) != 1 {
		t.Errorf("newer scheme: got %+v", got)
	}
	// The same version under the new scheme is current.
	if got := check(t, idx, prefix(t, keg{name: "a", version: "1.0", scheme: 1, linked: true})); len(got) != 0 {
		t.Errorf("same scheme and version: got %+v", got)
	}
	// A keg on a newer scheme than the formula counts as current.
	old := index(t, formula{name: "a", version: "5.0", scheme: 0})
	if got := check(t, old, prefix(t, keg{name: "a", version: "1.0", scheme: 1, linked: true})); len(got) != 0 {
		t.Errorf("keg scheme ahead: got %+v", got)
	}
}

func TestHeadKegIsNotOutdated(t *testing.T) {
	// Unlinked, so only the HEAD rule keeps it from being reported.
	got := check(t, index(t, formula{name: "jq", version: "1.8"}), prefix(t, keg{name: "jq", version: "HEAD-abc123"}))
	if len(got) != 0 {
		t.Errorf("got %+v", got)
	}
}

func TestPinnedFormulaIsListedWithItsPin(t *testing.T) {
	p := prefix(t, keg{name: "jq", version: "1.7", linked: true, pinned: true})
	got := check(t, index(t, formula{name: "jq", version: "1.8"}), p)
	want := []Result{{Name: "jq", InstalledVersions: []string{"1.7"}, CurrentVersion: "1.8", Pinned: true, PinnedVersion: "1.7"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestUnknownFormulaIsSkipped(t *testing.T) {
	p := prefix(t, keg{name: "tapthing", version: "1", linked: true}, keg{name: "jq", version: "1.7", linked: true})
	got := check(t, index(t, formula{name: "jq", version: "1.8"}), p)
	if !reflect.DeepEqual(names(got), []string{"jq"}) {
		t.Errorf("got %v", names(got))
	}
}

func TestSortedByName(t *testing.T) {
	p := prefix(t, keg{name: "zsh", version: "1", linked: true}, keg{name: "age", version: "1", linked: true}, keg{name: "jq", version: "1", linked: true})
	idx := index(t, formula{name: "zsh", version: "2"}, formula{name: "age", version: "2"}, formula{name: "jq", version: "2"})
	if got := names(check(t, idx, p)); !reflect.DeepEqual(got, []string{"age", "jq", "zsh"}) {
		t.Errorf("got %v", got)
	}
}

func TestKegWithoutReceiptStillChecked(t *testing.T) {
	p := prefix(t, keg{name: "jq", version: "1.7", linked: true})
	must(t, os.Remove(filepath.Join(string(p), "Cellar/jq/1.7/INSTALL_RECEIPT.json")))
	if got := check(t, index(t, formula{name: "jq", version: "1.8"}), p); len(got) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestUnparseableKegVersionCountsAsNotCurrent(t *testing.T) {
	p := prefix(t, keg{name: "jq", version: " ", linked: true})
	got := check(t, index(t, formula{name: "jq", version: "1.8"}), p)
	if len(got) != 1 {
		t.Errorf("got %+v", got)
	}
}

func TestTapKegWithACoreNamesakeIsSkipped(t *testing.T) {
	idx := index(t, formula{name: "opencode", version: "2.0"})
	p := prefix(t, keg{name: "opencode", version: "1.19", tap: "anomalyco/tap", linked: true})
	if got := check(t, idx, p); len(got) != 0 {
		t.Errorf("a tap install was compared with the core formula: %+v", got)
	}
	core := prefix(t, keg{name: "opencode", version: "1.19", tap: "homebrew/core", linked: true})
	if got := check(t, idx, core); len(got) != 1 {
		t.Errorf("a core install should be checked: %+v", got)
	}
}
