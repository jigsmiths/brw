package api

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
)

// tapLoader serves formulae of a tap by name, counting loads.
func tapLoader(tap string, loads *int, broken ...string) TapLoader {
	return func(name string) (Formula, json.RawMessage, error) {
		*loads++
		if slices.Contains(broken, name) {
			return Formula{}, nil, errors.New("unsupported Ruby")
		}
		f := Formula{Name: name, FullName: tap + "/" + name, Tap: tap}
		f.Versions.Stable = "1.0"
		raw, _ := json.Marshal(map[string]string{"name": name, "full_name": f.FullName})
		return f, raw, nil
	}
}

func coreIndex(names ...string) *Index {
	var fs []Formula
	for _, n := range names {
		fs = append(fs, Formula{Name: n, FullName: n, Tap: "homebrew/core"})
	}
	return NewIndex(fs)
}

func TestTapFormulaeLoadOnFirstLookup(t *testing.T) {
	idx := coreIndex("jq")
	var loads int
	idx.AddTap("u/t", []string{"tool", "jq"}, nil, tapLoader("u/t", &loads))
	if loads != 0 {
		t.Fatalf("loaded %d before any lookup", loads)
	}
	f, ok := idx.Lookup("tool")
	if !ok || f.FullName != "u/t/tool" {
		t.Fatalf("tool: %+v %v", f, ok)
	}
	idx.Lookup("u/t/tool")
	if loads != 1 {
		t.Errorf("loads = %d, want 1", loads)
	}
}

func TestLookupPrefersCoreForABareName(t *testing.T) {
	idx := coreIndex("jq")
	var loads int
	idx.AddTap("u/t", []string{"jq"}, nil, tapLoader("u/t", &loads))
	if f, ok := idx.Lookup("jq"); !ok || f.Tap != "homebrew/core" {
		t.Errorf("jq: %+v", f)
	}
	if f, ok := idx.Lookup("u/t/jq"); !ok || f.Tap != "u/t" {
		t.Errorf("u/t/jq: %+v", f)
	}
	if f, ok := idx.LookupFrom("u/t", "jq"); !ok || f.Tap != "u/t" {
		t.Errorf("LookupFrom: %+v", f)
	}
	if f, ok := idx.LookupFrom("homebrew/core", "jq"); !ok || f.Tap != "homebrew/core" {
		t.Errorf("LookupFrom core: %+v", f)
	}
}

func TestABareNameInTwoTapsIsAmbiguous(t *testing.T) {
	idx := coreIndex()
	var loads int
	idx.AddTap("a/t", []string{"tool"}, nil, tapLoader("a/t", &loads))
	idx.AddTap("b/t", []string{"tool"}, nil, tapLoader("b/t", &loads))
	if _, ok := idx.Lookup("tool"); ok {
		t.Error("a name two taps have resolved")
	}
	if got := idx.TapsWith("tool"); !slices.Equal(got, []string{"a/t/tool", "b/t/tool"}) {
		t.Errorf("TapsWith = %v", got)
	}
	if _, ok := idx.Lookup("b/t/tool"); !ok {
		t.Error("the full name should resolve")
	}
}

func TestAliasesAndOldNamesOfATapFormula(t *testing.T) {
	idx := coreIndex()
	var loads int
	idx.AddTap("u/t", []string{"tool"}, map[string]string{"tool2": "tool", "oldtool": "tool"}, tapLoader("u/t", &loads))
	for _, n := range []string{"tool2", "oldtool", "u/t/tool2"} {
		if f, ok := idx.Lookup(n); !ok || f.Name != "tool" {
			t.Errorf("%s: %+v %v", n, f, ok)
		}
	}
}

func TestATapFormulaThatCannotLoadSaysWhy(t *testing.T) {
	idx := coreIndex()
	var loads int
	idx.AddTap("u/t", []string{"bad", "good"}, nil, tapLoader("u/t", &loads, "bad"))
	if _, ok := idx.Lookup("bad"); ok {
		t.Error("a broken formula resolved")
	}
	if err := idx.LoadError("u/t/bad"); err == nil {
		t.Error("no load error")
	}
	var names []string
	for _, f := range idx.Formulae() {
		names = append(names, f.FullName)
	}
	if !slices.Equal(names, []string{"u/t/good"}) {
		t.Errorf("Formulae = %v", names)
	}
}

func TestRawOfATapFormula(t *testing.T) {
	idx := coreIndex()
	var loads int
	idx.AddTap("u/t", []string{"tool"}, nil, tapLoader("u/t", &loads))
	raw, ok := idx.TapRaw("u/t/tool")
	if !ok || string(raw) != `{"full_name":"u/t/tool","name":"tool"}` {
		t.Errorf("raw = %s, %v", raw, ok)
	}
	if _, ok := idx.TapRaw("jq"); ok {
		t.Error("a core name has no tap hash")
	}
}
