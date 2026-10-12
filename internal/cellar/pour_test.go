package cellar

import (
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func read(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// brewReceipts are receipts brew wrote when it poured these bottles, with the
// manifest tab each one started from.
var brewReceipts = []struct {
	name, tab, receipt, stable string
	onRequest                  bool
	time                       int64
}{
	{"jq", "pour-jq.tab.json", "pour-jq.receipt.json", "1.8.2", true, 1791753509},
	{"oniguruma", "pour-onig.tab.json", "pour-onig.receipt.json", "6.9.10", false, 1791753508},
}

func TestPourReceiptMatchesBrewByteForByte(t *testing.T) {
	for _, tt := range brewReceipts {
		t.Run(tt.name, func(t *testing.T) {
			r, err := NewPourReceipt(read(t, tt.tab), Pour{
				OnRequest: tt.onRequest, Time: time.Unix(tt.time, 0), Aliases: nil,
				Stable: tt.stable, Tap: "homebrew/core", Path: "/cache/api/internal/packages.arm64_golden_gate.jws.json",
			})
			if err != nil {
				t.Fatal(err)
			}
			got, err := r.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			if want := read(t, tt.receipt); string(got) != string(want) {
				t.Errorf("receipt differs from brew's\ngot:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

func TestPourReceiptDropsWhatBrewDrops(t *testing.T) {
	// A key we do not model, as a future brew might add to a tab.
	var tab map[string]any
	if err := json.Unmarshal(read(t, "pour-jq.tab.json"), &tab); err != nil {
		t.Fatal(err)
	}
	tab["future_key"] = true
	// An older manifest has no relocation lists.
	delete(tab, "linkage_files")
	delete(tab, "binary_relocation_files")
	raw, err := json.Marshal(tab)
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewPourReceipt(raw, Pour{Stable: "1.8.2", Tap: "homebrew/core", Time: time.Unix(1, 0)})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Marshal()
	if err != nil {
		t.Fatalf("an unknown tab key must not block the install: %v", err)
	}
	for _, absent := range []string{"future_key", "linkage_files", "binary_relocation_files"} {
		if strings.Contains(string(got), absent) {
			t.Errorf("%s is in the receipt:\n%s", absent, got)
		}
	}
	for _, empty := range []string{`"used_options": []`, `"unused_options": []`, `"aliases": []`} {
		if !strings.Contains(string(got), empty) {
			t.Errorf("%s should be an empty list, not null:\n%s", empty, got)
		}
	}
}

func TestPourReceiptFields(t *testing.T) {
	r, err := NewPourReceipt(read(t, "pour-jq.tab.json"), Pour{
		OnRequest: true, Time: time.Unix(42, 0), Aliases: []string{"jaq"}, Stable: "1.8.2", Scheme: 3, Tap: "homebrew/core", Path: "/c/p.json",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !r.BuiltAsBottle || !r.PouredFromBottle || !r.LoadedFromAPI || r.LoadedFromInternalAPI == nil || !*r.LoadedFromInternalAPI {
		t.Errorf("flags = %+v", r)
	}
	if !r.InstalledOnRequest || r.InstalledAsDependency != nil || r.Time != 42 || !reflect.DeepEqual(r.Aliases, []string{"jaq"}) {
		t.Errorf("request/time/aliases = %v %v %d %v", r.InstalledOnRequest, r.InstalledAsDependency, r.Time, r.Aliases)
	}
	if r.Source.Tap != "homebrew/core" || r.VersionScheme() != 3 {
		t.Errorf("source tap %q scheme %d", r.Source.Tap, r.VersionScheme())
	}
	// What came from the bottle is kept as it was.
	if r.HomebrewVersion != "6.0.22-271-gaebd696" || r.Arch != "arm64" || len(r.RuntimeDependencies) != 1 || len(r.LinkageFiles) != 2 {
		t.Errorf("tab data was lost: %+v", r)
	}
	got, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseReceipt(got)
	if err != nil || back.VersionScheme() != 3 || back.Source.Tap != "homebrew/core" {
		t.Errorf("the receipt does not read back: %v", err)
	}
}

func TestPourReceiptRejectsABadTab(t *testing.T) {
	for _, tab := range []string{"", "not json", "[]"} {
		if _, err := NewPourReceipt([]byte(tab), Pour{}); err == nil {
			t.Errorf("NewPourReceipt(%q) should fail", tab)
		}
	}
	if _, err := NewPourReceipt([]byte("{}"), Pour{}); errors.Is(err, ErrUnknownKeys) {
		t.Errorf("an empty tab is fine: %v", err)
	}
}
