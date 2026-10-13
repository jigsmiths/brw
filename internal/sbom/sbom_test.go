package sbom

import (
	"os"
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

// installed is the install time brew recorded for the fzf whose SBOM the test data holds.
var installed = time.Unix(1791746767, 0)

const brewTool = "Tool: https://github.com/Homebrew/brew@7.0.9-60-gea07035"

func TestUpdateMatchesWhatBrewWritesByteForByte(t *testing.T) {
	got, ok := Update(read(t, "fzf.shipped.json"), Params{
		Created: installed, Creator: brewTool, Supplement: read(t, "fzf.supplement.json"), Tag: "arm64_golden_gate",
	})
	if !ok {
		t.Fatal("the SBOM was left alone")
	}
	if want := read(t, "fzf.brew.json"); string(got) != string(want) {
		t.Errorf("not what brew wrote\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpdateWithoutASupplementOnlyChangesWhoMadeItAndWhen(t *testing.T) {
	got, ok := Update(read(t, "fzf.shipped.json"), Params{
		Created: installed, Creator: "Tool: https://github.com/jigsmiths/brw@abc1234",
	})
	if !ok || string(got) != string(read(t, "fzf.nosupplement.json")) {
		t.Errorf("ok = %v, got:\n%s", ok, got)
	}
}

func TestFormattingFollowsRubyForTheCasesJSONGeneratorsDifferOn(t *testing.T) {
	n, err := parse(read(t, "edge.input.json"))
	if err != nil {
		t.Fatal(err)
	}
	got := n.pretty()
	if want := read(t, "edge.expected.json"); string(got) != string(want) {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpdateTakesTheSupplementOfTheBottleTag(t *testing.T) {
	sbom := `{"creationInfo":{"created":"x","creators":["y"]},"packages":[{"name":"base"}],"relationships":[],"documentDescribes":[]}`
	tagged := `{"tags":{"arm64_tahoe":{"packages":[{"name":"tahoe"}]},"x86_64_linux":{"packages":[{"name":"linux"}]}}}`
	for tag, want := range map[string]string{"arm64_tahoe": `"tahoe"`, "x86_64_linux": `"linux"`} {
		got, ok := Update([]byte(sbom), Params{Created: installed, Creator: "c", Supplement: []byte(tagged), Tag: tag})
		if !ok || !strings.Contains(string(got), want) || strings.Count(string(got), `"name"`) != 2 {
			t.Errorf("%s: ok = %v\n%s", tag, ok, got)
		}
	}
	got, ok := Update([]byte(sbom), Params{Created: installed, Creator: "c", Supplement: []byte(tagged), Tag: "arm64_sonoma"})
	if !ok || strings.Count(string(got), `"name"`) != 1 {
		t.Errorf("a tag the supplement has nothing for adds nothing: ok = %v\n%s", ok, got)
	}
}

func TestUpdateMergesOnlyTheThreeListsAndOnlyWhenBothAreArrays(t *testing.T) {
	sbom := `{"creationInfo":{"created":"x","creators":["y"]},"documentDescribes":["a"],"packages":[{"name":"base"}],"relationships":"not a list","name":"doc","files":["f0"]}`
	supp := `{"documentDescribes":["b"],"packages":[{"name":"extra"}],"relationships":[{"r":1}],"name":"other","files":["f1"]}`
	got, _ := Update([]byte(sbom), Params{Created: installed, Creator: "c", Supplement: []byte(supp)})
	s := string(got)
	for _, want := range []string{`"b"`, `"extra"`, `"name": "doc"`, `"relationships": "not a list"`} {
		if !strings.Contains(s, want) {
			t.Errorf("%s is missing:\n%s", want, s)
		}
	}
	for _, absent := range []string{`"r"`, `"other"`, `"f1"`} {
		if strings.Contains(s, absent) {
			t.Errorf("%s was merged:\n%s", absent, s)
		}
	}
	if strings.Index(s, `"a"`) > strings.Index(s, `"b"`) || strings.Index(s, `"base"`) > strings.Index(s, `"extra"`) {
		t.Errorf("the supplement goes after what the bottle had:\n%s", s)
	}
}

func TestUpdateIgnoresASupplementItCannotUse(t *testing.T) {
	sbom := `{"creationInfo":{"created":"x","creators":["y"]},"packages":[{"name":"base"}]}`
	for name, supp := range map[string]string{"not json": `{oops`, "an array": `[1]`, "a string": `"x"`, "empty": ``, "null": `null`, "bad tags": `{"tags":[1]}`} {
		got, ok := Update([]byte(sbom), Params{Created: installed, Creator: "c", Supplement: []byte(supp), Tag: "t"})
		if !ok || strings.Count(string(got), `"name"`) != 1 || !strings.Contains(string(got), `"created": "2026-10-11T19:26:07Z"`) {
			t.Errorf("%s: the SBOM should still be updated: ok = %v\n%s", name, ok, got)
		}
	}
}

func TestUpdateLeavesAnSBOMItCannotReadAlone(t *testing.T) {
	for name, in := range map[string]string{
		"not json": `{nope`, "an array": `[]`, "empty": ``, "trailing text": `{"creationInfo":{"created":"x","creators":[]}} junk`,
		"no creationInfo": `{"packages":[]}`, "creationInfo is a list": `{"creationInfo":[]}`, "creationInfo is null": `{"creationInfo":null}`,
	} {
		got, ok := Update([]byte(in), Params{Created: installed, Creator: "c"})
		if ok || string(got) != in {
			t.Errorf("%s: ok = %v, got %q", name, ok, got)
		}
	}
}

func TestUpdateKeepsKeyOrderAndAddsMissingKeysAtTheEnd(t *testing.T) {
	got, ok := Update([]byte(`{"z":1,"creationInfo":{"licenseListVersion":"3.2","creators":["old"],"comment":"c"},"a":2}`),
		Params{Created: installed, Creator: "c"})
	want := "{\n  \"z\": 1,\n  \"creationInfo\": {\n    \"licenseListVersion\": \"3.2\",\n    \"creators\": [\n      \"c\"\n    ],\n    \"comment\": \"c\",\n    \"created\": \"2026-10-11T19:26:07Z\"\n  },\n  \"a\": 2\n}"
	if !ok || string(got) != want {
		t.Errorf("got:\n%s\nwant:\n%s", got, want)
	}
}

func TestUpdateWritesTheTimeInUTCWithoutFractions(t *testing.T) {
	in := `{"creationInfo":{"created":"x","creators":[]}}`
	loc := time.FixedZone("far east", 9*3600)
	got, _ := Update([]byte(in), Params{Created: time.Date(2026, 1, 2, 12, 30, 45, 999000000, loc), Creator: "c"})
	if !strings.Contains(string(got), `"created": "2026-01-02T03:30:45Z"`) {
		t.Errorf("got:\n%s", got)
	}
}

func TestUpdateKeepsNumbersAndLiteralsAsTheyWere(t *testing.T) {
	in := `{"creationInfo":{"created":"x","creators":[]},"n":[12345678901234567890,-0,7,true,false,null]}`
	got, _ := Update([]byte(in), Params{Created: installed, Creator: "c"})
	for _, want := range []string{"12345678901234567890", "-0", "true", "false", "null"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%s was changed:\n%s", want, got)
		}
	}
}

func TestUpdateUsesTheWholeSupplementWhenItsTagsAreNotAnObject(t *testing.T) {
	sbom := `{"creationInfo":{"created":"x","creators":["y"]},"packages":[{"name":"base"}]}`
	got, _ := Update([]byte(sbom), Params{Created: installed, Creator: "c", Tag: "t",
		Supplement: []byte(`{"tags":[1],"packages":[{"name":"whole"}]}`)})
	if !strings.Contains(string(got), `"whole"`) {
		t.Errorf("tags that are not an object do not make the supplement a per-tag one:\n%s", got)
	}
}

func TestUpdateSkipsAListTheBottleDoesNotHave(t *testing.T) {
	sbom := `{"creationInfo":{"created":"x","creators":["y"]},"name":"doc"}`
	got, ok := Update([]byte(sbom), Params{Created: installed, Creator: "c", Supplement: []byte(`{"packages":[{"name":"extra"}],"relationships":[1]}`)})
	if !ok || strings.Contains(string(got), "extra") || strings.Contains(string(got), `"packages"`) {
		t.Errorf("a list that is not there is not made: ok = %v\n%s", ok, got)
	}
}
