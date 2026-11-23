package sbom

import (
	"strings"
	"testing"
	"time"
)

func TestBuiltMatchesWhatBrewWritesByteForByte(t *testing.T) {
	got, err := Built(BuildParams{
		Name: "flux", FullName: "fluxcd/tap/flux", Version: "2.10.1",
		URL:      "https://github.com/fluxcd/flux2/releases/download/v2.10.1/flux_2.10.1_darwin_arm64.tar.gz",
		SHA256:   "c0a8dc285c568905ae38e25074985f3181c82a3c8d66316959561f27a626909d",
		Compiler: "clang", SourceModified: time.Unix(1795179950, 0), Creator: "Tool: https://github.com/Homebrew/brew",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := read(t, "build-flux.spdx.json"); string(got) != string(want) {
		t.Errorf("not what brew wrote\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A HEAD build names its repository and has no checksum.
func TestBuiltFromHeadMatchesBrew(t *testing.T) {
	got, err := Built(BuildParams{
		Name: "headtool", FullName: "u/test/headtool", Version: "HEAD-cf6a8f9", URL: "https://example.com/headtool.git",
		License: "MIT", Compiler: "clang", SourceModified: time.Unix(1795440985, 0), Creator: "Tool: https://github.com/Homebrew/brew",
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := read(t, "build-headtool-head.spdx.json"); string(got) != string(want) {
		t.Errorf("not what brew wrote\ngot:\n%s\nwant:\n%s", got, want)
	}
}

// A runtime dependency is listed with the bottle it was poured from, and its
// URL only when the installed version is the formula's current one.
func TestBuiltListsRuntimeDependencies(t *testing.T) {
	got, err := Built(BuildParams{
		Name: "tool", FullName: "u/t/tool", Version: "1.0", URL: "https://example.com/tool-1.0.tar.gz", SHA256: "aa",
		License: "MIT", Compiler: "clang", Xcode: "27.0", SourceModified: time.Unix(0, 0), Creator: "Tool: brw",
		Deps: []Dep{
			{Name: "app", FullName: "app", PkgVersion: "1.0", FormulaPkgVersion: "1.0", License: "Apache-2.0", BottleURL: "https://b/app", BottleSHA256: "bb"},
			{Name: "old", FullName: "old", PkgVersion: "1.0", FormulaPkgVersion: "2.0", BottleURL: "https://b/old", BottleSHA256: "cc"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(got)
	for _, want := range []string{
		// spdx_id escapes the dot of the version too
		`"SPDXID": "SPDXRef-Package-SPDXRef-app-1.2e.0"`, `"licenseConcluded": "Apache-2.0"`, `"downloadLocation": "https://b/app"`,
		`"SPDXID": "SPDXRef-Package-SPDXRef-old-1.2e.0"`, `"relationshipType": "RUNTIME_DEPENDENCY_OF"`,
		`"versionInfo": "27.0"`, `"licenseConcluded": "MIT"`, `"referenceLocator": "pkg:brew/u/t/tool@1.0"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("lacks %s:\n%s", want, s)
		}
	}
	if strings.Contains(s, "https://b/old") {
		t.Error("a dependency installed at an older version names the current bottle")
	}
}

func TestSPDXIDEscapesLikeBrew(t *testing.T) {
	if got := spdxID("Archive-c++@2-src"); got != "SPDXRef-Archive-c.2b..2b..40.2-src" {
		t.Errorf("got %s", got)
	}
}
