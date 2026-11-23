package cellar

import (
	"testing"
	"time"
)

func TestBuildReceiptMatchesBrewByteForByte(t *testing.T) {
	r, err := NewBuildReceipt(Build{
		HomebrewVersion: "7.0.15-111-ge40c695", OnRequest: true, Time: time.Unix(1795420582, 0),
		SourceModified: time.Unix(1795109067, 0), Tap: "charmbracelet/tap",
		TapGitHead: "279dec178983eb547a45fd7aa46c40856daa1c94", Path: "/prefix/Library/Taps/charmbracelet/homebrew-tap/crush.rb",
		Stable: "0.102.0", Arch: "arm64",
		BuiltOn: BuiltOn{OS: "Macintosh", OSVersion: "macOS 27.2", CPUFamily: "arm_firestorm_icestorm",
			CLT: "27.2.0.0.1792721180", PreferredPerl: "5.34"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if want := read(t, "build-crush.receipt.json"); string(got) != string(want) {
		t.Errorf("receipt differs from brew's\ngot:\n%s\nwant:\n%s", got, want)
	}
	if r.Source.Tap != "charmbracelet/tap" || r.PouredFromBottle || !r.InstalledOnRequest {
		t.Errorf("parsed back: %+v", r)
	}
}
