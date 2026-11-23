package cellar

import (
	"testing"
	"time"
)

var brewMachine = BuiltOn{OS: "Macintosh", OSVersion: "macOS 27.2", CPUFamily: "arm_firestorm_icestorm",
	CLT: "27.2.0.0.1792721180", PreferredPerl: "5.34"}

func TestBuildReceiptMatchesBrewByteForByte(t *testing.T) {
	r, err := NewBuildReceipt(Build{
		HomebrewVersion: "7.0.15-111-ge40c695", OnRequest: true, Time: time.Unix(1795420582, 0),
		SourceModified: time.Unix(1795109067, 0), Tap: "charmbracelet/tap",
		TapGitHead: "279dec178983eb547a45fd7aa46c40856daa1c94", Path: "/prefix/Library/Taps/charmbracelet/homebrew-tap/crush.rb",
		Stable: "0.102.0", Arch: "arm64", BuiltOn: brewMachine,
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

func TestBuildReceiptOfAFormulaWithAHead(t *testing.T) {
	r, err := NewBuildReceipt(Build{
		HomebrewVersion: "7.0.15-111-ge40c695", OnRequest: true, Time: time.Unix(1795440991, 0),
		SourceModified: time.Unix(1795440985, 0), Tap: "u/test",
		TapGitHead: "dc4d4c25060478139e0c3ad06e288de3061ae970", Path: "/prefix/Library/Taps/u/homebrew-test/Formula/headtool.rb",
		Stable: "1.0", Head: "HEAD", Arch: "arm64", BuiltOn: brewMachine,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if want := read(t, "build-headtool.receipt.json"); string(got) != string(want) {
		t.Errorf("receipt differs from brew's\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestBuildReceiptOfAHeadBuild(t *testing.T) {
	r, err := NewBuildReceipt(Build{
		HomebrewVersion: "7.0.15-111-ge40c695", OnRequest: true, Time: time.Unix(1795441004, 0),
		SourceModified: time.Unix(1795440985, 0), Tap: "u/test",
		TapGitHead: "dc4d4c25060478139e0c3ad06e288de3061ae970", Path: "/prefix/Library/Taps/u/homebrew-test/Formula/headtool.rb",
		Stable: "1.0", Head: "HEAD-cf6a8f9", HeadBuild: true, SCMRevision: "cf6a8f90dfb2d66d7199bde25e5f521751a7f98b",
		Arch: "arm64", BuiltOn: brewMachine,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if want := read(t, "build-headtool-head.receipt.json"); string(got) != string(want) {
		t.Errorf("receipt differs from brew's\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestReceiptSpecAndStableVersion(t *testing.T) {
	for file, want := range map[string]struct {
		spec   Spec
		stable string
	}{
		"build-headtool-head.receipt.json": {SpecHead, "1.0"}, "build-crush.receipt.json": {SpecStable, "0.102.0"},
	} {
		r, err := ParseReceipt(read(t, file))
		if err != nil {
			t.Fatal(err)
		}
		if r.Source.Spec != want.spec || r.Source.StableAtInstall != want.stable || r.IsHeadBuild() != (want.spec == SpecHead) {
			t.Errorf("%s: spec %q, stable %q; want %+v", file, r.Source.Spec, r.Source.StableAtInstall, want)
		}
	}
}
