package rb

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// formulaFields are the parts of brew's formula hash a formula file decides.
var formulaFields = strings.Fields(`name desc license homepage versions urls patches revision version_scheme
compatibility_version bottle pour_bottle_only_if keg_only keg_only_reason options build_dependencies dependencies
test_dependencies recommended_dependencies optional_dependencies no_linkage_dependencies uses_from_macos
uses_from_macos_bounds head_dependencies requirements conflicts_with conflicts_with_reasons link_overwrite caveats
deprecated deprecation_date deprecation_reason deprecation_replacement_formula deprecation_replacement_cask
deprecate_args disabled disable_date disable_reason disable_replacement_formula disable_replacement_cask
disable_args post_install_steps post_install_defined service`)

// formulaTags are OnSystem::VALID_OS_ARCH_TAGS, which brew evaluates variations on.
var formulaTags = strings.Fields(`arm64_golden_gate golden_gate arm64_tahoe tahoe arm64_sequoia sequoia arm64_sonoma
sonoma arm64_ventura ventura arm64_monterey monterey arm64_big_sur big_sur arm64_linux x86_64_linux`)

// asBrewHost pins the machine the fixtures were made on: an arm64 Mac on
// macOS 27, whose user is user.
func asBrewHost(t *testing.T, user string) {
	t.Helper()
	asBrew(t)
	oldHost, oldUser := hostPlatform, lookupUser
	hostPlatform = func() *platform { return &platform{os: "macos", arch: "arm", macos: "27"} }
	lookupUser = func() string { return user }
	t.Cleanup(func() { hostPlatform, lookupUser = oldHost, oldUser })
}

// compareFormula evaluates src on every tag and returns how it differs from
// want, brew's hash with its prefix replaced by the placeholder.
func compareFormula(t *testing.T, src []byte, name string, want map[string]any) []string {
	t.Helper()
	var diffs []string
	for _, tag := range formulaTags {
		f, err := LoadFormula(src, name, tag)
		if err != nil {
			return []string{tag + ": " + err.Error()}
		}
		b, err := json.Marshal(f)
		if err != nil {
			return []string{tag + ": " + err.Error()}
		}
		var got map[string]any
		if err := json.Unmarshal(b, &got); err != nil {
			t.Fatal(err)
		}
		exp := maps.Clone(want)
		if vars, _ := want["variations"].(map[string]any); vars != nil {
			if v, _ := vars[bottleTag(tag)].(map[string]any); v != nil {
				maps.Copy(exp, v)
			}
		}
		for _, k := range formulaFields {
			if !reflect.DeepEqual(got[k], exp[k]) {
				gb, _ := json.Marshal(got[k])
				eb, _ := json.Marshal(exp[k])
				diffs = append(diffs, tag+" "+k+":\n  got:  "+string(gb)+"\n  want: "+string(eb))
			}
		}
	}
	return diffs
}

// readFormulaWant reads brew info --json=v2 --variations output, with the
// prefix it ran in replaced by the placeholder.
func readFormulaWant(t *testing.T, path string) (map[string]any, string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Formulae []map[string]any `json:"formulae"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || len(doc.Formulae) != 1 {
		t.Fatalf("%s: %v", path, err)
	}
	f := doc.Formulae[0]
	prefix := os.Getenv("BRW_FORMULA_PREFIX")
	if prefix != "" {
		b = []byte(strings.ReplaceAll(string(b), prefix, PrefixPlaceholder))
		_ = json.Unmarshal(b, &doc)
		f = doc.Formulae[0]
	}
	name, _ := f["name"].(string)
	return f, name
}

// The fixtures are formula files from taps with brew info --json=v2
// --variations for them, made on an arm64 Mac running macOS 27.
func TestFormulaMatchesBrew(t *testing.T) {
	asBrewHost(t, "brewuser")
	files, _ := filepath.Glob(filepath.Join("testdata", "formula", "*.json"))
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, jf := range files {
		t.Run(filepath.Base(jf), func(t *testing.T) {
			want, name := readFormulaWant(t, jf)
			src, err := os.ReadFile(strings.TrimSuffix(jf, ".json") + ".rb")
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range compareFormula(t, src, name, want) {
				t.Error(d)
			}
		})
	}
}

func TestLoadFormulaRefusesRubyItCannotTranslate(t *testing.T) {
	asBrewHost(t, "brewuser")
	for name, src := range map[string]string{
		"tap-local Ruby":     "require_relative \"../lib/strategy\"\nclass Foo < Formula\n  url \"https://example.com/foo-1.0.tar.gz\"\nend\n",
		"globals":            "class Foo < Formula\n  $x = 1\nend\n",
		"other superclasses": "class Foo < AwsFormula\n  url \"https://example.com/foo-1.0.tar.gz\"\nend\n",
		"no url":             "class Foo < Formula\n  desc \"x\"\nend\n",
		"unknown DSL":        "class Foo < Formula\n  url \"https://example.com/foo-1.0.tar.gz\"\n  go_resource \"x\"\nend\n",
		"rescue":             "class Foo < Formula\n  url \"https://example.com/foo-1.0.tar.gz\"\n  begin\n    x\n  rescue\n  end\nend\n",
	} {
		if _, err := LoadFormula([]byte(src), "foo", "arm64_golden_gate"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", name, err)
		}
	}
}

func TestCaveatsAnswerOnSystemBlocks(t *testing.T) {
	asBrewHost(t, "brewuser")
	src := "class Foo < Formula\n  url \"https://example.com/foo-1.0.tar.gz\"\n  def caveats\n    on_macos do\n      \"mac\"\n    end\n  end\nend\n"
	for tag, want := range map[string]any{"arm64_golden_gate": "mac", "x86_64_linux": nil} {
		f, err := LoadFormula([]byte(src), "foo", tag)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatal(err)
		}
		var h map[string]any
		_ = json.Unmarshal(b, &h)
		if h["caveats"] != want {
			t.Errorf("%s: caveats %v, want %v", tag, h["caveats"], want)
		}
	}
}

// def caveats runs only when the hash is made; Ruby it cannot run there is
// refused like any other.
func TestCaveatsRubyItCannotRunIsRefused(t *testing.T) {
	asBrewHost(t, "brewuser")
	src := "class Foo < Formula\n  url \"https://example.com/foo-1.0.tar.gz\"\n  def caveats\n    File.read(\"/etc/x\")\n  end\nend\n"
	f, err := LoadFormula([]byte(src), "foo", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := json.Marshal(f); !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

func TestLoadFormulaDetectsTheVersionFromTheURL(t *testing.T) {
	asBrewHost(t, "brewuser")
	f, err := LoadFormula([]byte("class Foo < Formula\n  url \"https://example.com/foo-1.2.3.tar.gz\"\n  revision 2\nend\n"), "foo", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	if got := f.b.pkgVersion(); got != "1.2.3_2" {
		t.Errorf("pkg version %q", got)
	}
}

// TestConformanceTapFormulae compares every formula in a directory of
// <name>.rb and <name>.json pairs, made with brew info --json=v2 --variations
// in the prefix BRW_FORMULA_PREFIX. It reports how many match and why the
// rest do not.
func TestConformanceTapFormulae(t *testing.T) {
	dir := os.Getenv("BRW_FORMULA_GOLDEN")
	if dir == "" {
		t.Skip("set BRW_FORMULA_GOLDEN and BRW_FORMULA_PREFIX to compare a directory of tap formulae with brew")
	}
	asBrewHost(t, os.Getenv("USER"))
	files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var match, refused int
	why := map[string]int{}
	for _, jf := range files {
		want, name := readFormulaWant(t, jf)
		src, err := os.ReadFile(strings.TrimSuffix(jf, ".json") + ".rb")
		if err != nil {
			t.Fatal(err)
		}
		diffs := compareFormula(t, src, name, want)
		if len(diffs) == 0 {
			match++
			continue
		}
		if strings.Contains(diffs[0], "unsupported") {
			refused++
			_, msg, _ := strings.Cut(diffs[0], "unsupported: ")
			why["refused: "+msg]++
		} else {
			why[strings.SplitN(strings.TrimSpace(strings.SplitN(diffs[0], ":", 2)[0]), " ", 2)[1]]++
		}
		if testing.Verbose() {
			t.Logf("%s:\n%s", filepath.Base(jf), strings.Join(diffs[:min(len(diffs), 3)], "\n"))
		}
	}
	keys := make([]string, 0, len(why))
	for k := range why {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return why[keys[i]] > why[keys[j]] })
	for _, k := range keys {
		t.Logf("%4d %s", why[k], k)
	}
	t.Logf("%d of %d match brew, %d refused", match, len(files), refused)
}
