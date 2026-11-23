package rb

import (
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

var fields = strings.Fields(`name desc homepage url url_specs version sha256 artifacts caveats caveats_rosetta
depends_on conflicts_with container rename auto_updates deprecated deprecation_date deprecation_reason
deprecation_replacement_formula deprecation_replacement_cask disabled disable_date disable_reason
disable_replacement_formula disable_replacement_cask languages`)

var macTags = strings.Fields(`arm64_golden_gate golden_gate arm64_tahoe tahoe arm64_sequoia sequoia arm64_sonoma sonoma
arm64_ventura ventura arm64_monterey monterey arm64_big_sur big_sur`)

func asBrew(t *testing.T) {
	t.Helper()
	oldToday, oldEnv := today, lookupEnv
	today = func() string { return "2026-11-21" }
	lookupEnv = func(string) (string, bool) { return "", false }
	t.Cleanup(func() { today, lookupEnv = oldToday, oldEnv })
}

func tagsFor(want map[string]any) []string {
	vars, _ := want["variations"].(map[string]any)
	sp, _ := want["supported_platforms"].([]any)
	if len(vars) == 0 || len(sp) == 0 {
		return []string{"arm64_golden_gate"}
	}
	var tags []string
	for _, tag := range macTags {
		if slices.Contains(sp, any(tag)) {
			tags = append(tags, tag)
		}
	}
	return tags
}

func normal(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

func sameKindSorted(v any) any {
	list, _ := v.([]any)
	out := slices.Clone(list)
	key := func(a any) (int, string) {
		for k := range a.(map[string]any) {
			if k != "target" {
				b, _ := json.Marshal(a)
				return artifactOrder[k], string(b)
			}
		}
		return 0, ""
	}
	sort.SliceStable(out, func(i, j int) bool {
		gi, si := key(out[i])
		gj, sj := key(out[j])
		if gi != gj {
			return gi < gj
		}
		return si < sj
	})
	return out
}

func compareWithBrew(t *testing.T, src []byte, want map[string]any) []string {
	t.Helper()
	var diffs []string
	for _, tag := range tagsFor(want) {
		c, err := evaluate(src, tag)
		if err != nil {
			return []string{tag + ": " + err.Error()}
		}
		got := normal(c.hash(true)).(map[string]any)
		exp := map[string]any{}
		maps.Copy(exp, want)
		if vars, _ := want["variations"].(map[string]any); vars != nil {
			if v, _ := vars[tag].(map[string]any); v != nil {
				maps.Copy(exp, v)
			}
		}
		for _, f := range fields {
			g, e := got[f], exp[f]
			if f == "artifacts" {
				g, e = sameKindSorted(g), sameKindSorted(e)
			}
			if !reflect.DeepEqual(g, e) {
				gb, _ := json.Marshal(got[f])
				eb, _ := json.Marshal(exp[f])
				diffs = append(diffs, tag+" "+f+":\n  got:  "+string(gb)+"\n  want: "+string(eb))
			}
		}
	}
	return diffs
}

func readWant(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	if err := json.Unmarshal(b, &want); err != nil {
		t.Fatal(err)
	}
	return want
}

func TestEvaluateMatchesBrew(t *testing.T) {
	asBrew(t)
	files, _ := filepath.Glob(filepath.Join("testdata", "cask", "*.rb"))
	tapFiles, _ := filepath.Glob(filepath.Join("testdata", "tap", "*.rb"))
	files = append(files, tapFiles...)
	if len(files) == 0 {
		t.Fatal("no fixtures")
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			src, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			want := readWant(t, strings.TrimSuffix(f, ".rb")+".json")
			for _, d := range compareWithBrew(t, src, want) {
				t.Error(d)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	src := []byte(`cask "tool" do
  arch arm: "arm64", intel: "x86_64"

  version "1.2.3,45"
  sha256 arm:   "AAAA",
         intel: "bbbb"

  url "https://example.com/tool-#{version.csv.first}-#{arch}.zip",
      verified: "example.com/"
  name "Tool"
  desc "Does things"
  homepage "https://example.com/"

  livecheck do
    url :url
    strategy :github_latest do |json, regex|
      json["tag_name"]&.match(regex) { |m| m[1] }
    end
  end

  depends_on macos: :sonoma

  on_intel do
    caveats do
      requires_rosetta
    end
  end

  app "Tool.app"
  binary "#{appdir}/Tool.app/Contents/MacOS/tool"

  preflight do
    system_command "/usr/bin/xattr", args: ["-dr", "com.apple.quarantine", "#{staged_path}/tool"] if OS.mac?
  end

  zap trash: "~/Library/Tool"
end
`)
	arm, err := LoadCask(src, "arm64_tahoe")
	if err != nil {
		t.Fatal(err)
	}
	if arm.Token != "tool" || arm.Version != "1.2.3,45" || arm.SHA256 != "aaaa" || arm.Desc != "Does things" ||
		arm.URL != "https://example.com/tool-1.2.3-arm64.zip" {
		t.Errorf("arm: %+v", arm)
	}
	if arm.DependsOn.MacOS == nil || arm.DependsOn.MacOS.Comparator != ">=" || !slices.Equal(arm.DependsOn.MacOS.Versions, []string{"14"}) {
		t.Errorf("depends_on macos: %+v", arm.DependsOn.MacOS)
	}
	if arm.CaveatsRosetta {
		t.Error("caveats_rosetta on arm")
	}
	var kinds []string
	for _, a := range arm.Artifacts {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(a, &m); err != nil {
			t.Fatal(err)
		}
		for k := range m {
			if k != "target" {
				kinds = append(kinds, k)
			}
		}
	}
	if want := []string{"preflight_steps", "app", "binary", "zap"}; !slices.Equal(kinds, want) {
		t.Errorf("artifacts %q, want %q", kinds, want)
	}
	if got := string(arm.Artifacts[0]); !strings.Contains(got, `"command":{"path":"/usr/bin/xattr"}`) ||
		!strings.Contains(got, `"$HOMEBREW_PREFIX/Caskroom/tool/1.2.3,45/tool"`) || !strings.Contains(got, `"allow_failure":true`) {
		t.Errorf("preflight steps: %s", got)
	}
	if got := string(arm.Artifacts[2]); !strings.Contains(got, `"$APPDIR/Tool.app/Contents/MacOS/tool"`) {
		t.Errorf("binary: %s", got)
	}

	intel, err := LoadCask(src, "sonoma")
	if err != nil {
		t.Fatal(err)
	}
	if intel.SHA256 != "bbbb" || intel.URL != "https://example.com/tool-1.2.3-x86_64.zip" || !intel.CaveatsRosetta {
		t.Errorf("intel: %+v", intel)
	}
}

func TestLoadTranslatesFileOperations(t *testing.T) {
	src := []byte(`cask "agent" do
  version "1"
  postflight do
    plist = "#{Dir.home}/Library/LaunchAgents/agent.plist"
    File.write(plist, "<plist/>")
    system "launchctl", "load", plist
  end
  uninstall_preflight do
    plist = "#{Dir.home}/Library/LaunchAgents/agent.plist"
    if File.exist?(plist)
      system "launchctl", "unload", plist
      File.delete(plist)
    end
  end
end
`)
	c, err := LoadCask(src, "arm64_tahoe")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, a := range c.Artifacts {
		got = append(got, string(a))
	}
	want := []string{
		`{"uninstall_preflight_steps":[{"steps":[` +
			`{"command":{"path":"launchctl"},"guards":[{"path":"/$HOME/Library/LaunchAgents/agent.plist","condition":"if_exists","id":"1"}],"allow_failure":true,"type":"run","args":["unload","/$HOME/Library/LaunchAgents/agent.plist"]},` +
			`{"paths":[{"path":"/$HOME/Library/LaunchAgents/agent.plist"}],"guards":[{"path":"/$HOME/Library/LaunchAgents/agent.plist","condition":"if_exists","id":"1"}],"type":"remove"}]}]}`,
		`{"postflight_steps":[{"steps":[` +
			`{"path":{"path":"/$HOME/Library/LaunchAgents/agent.plist"},"type":"write","content":"<plist/>","overwrite":true},` +
			`{"command":{"path":"launchctl"},"allow_failure":true,"type":"run","args":["load","/$HOME/Library/LaunchAgents/agent.plist"]}]}]}`,
	}
	parse := func(ss []string) []any {
		var out []any
		for _, s := range ss {
			var v any
			if err := json.Unmarshal([]byte(s), &v); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}
	if !reflect.DeepEqual(parse(got), parse(want)) {
		t.Errorf("artifacts:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestLoadRefusesRubyItCannotTranslate(t *testing.T) {
	for name, src := range map[string]string{
		"FileUtils":  "cask \"t\" do\n  version \"1\"\n  postflight do\n    FileUtils.mv \"/tmp/x\", \"/tmp/y\"\n  end\nend\n",
		"file reads": "cask \"t\" do\n  version \"1\"\n  postflight do\n    File.read(\"/tmp/x\")\n  end\nend\n",
		"methods":    "cask \"t\" do\n  def helper\n  end\nend\n",
		"not a cask": "class Foo\nend\n",
		"garbled":    "cask \"t\" do\n  version \"1\n",
	} {
		if _, err := LoadCask([]byte(src), "arm64_tahoe"); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: %v, want ErrUnsupported", name, err)
		}
	}
}

// git clone https://github.com/Homebrew/homebrew-cask && curl -O https://formulae.brew.sh/api/cask.jws.json
// git -C homebrew-cask checkout <tap_git_head of any cask in the API>
// BRW_CASKRB_CASKS=homebrew-cask BRW_CASKRB_API=cask.jws.json go test -run Conformance ./internal/rb
func TestConformanceHomebrewCask(t *testing.T) {
	dir, apiPath := os.Getenv("BRW_CASKRB_CASKS"), os.Getenv("BRW_CASKRB_API")
	if dir == "" || apiPath == "" {
		t.Skip("set BRW_CASKRB_CASKS and BRW_CASKRB_API to compare with every cask in homebrew/cask")
	}
	asBrew(t)
	raw, err := os.ReadFile(apiPath)
	if err != nil {
		t.Fatal(err)
	}
	var jws struct{ Payload string }
	if err := json.Unmarshal(raw, &jws); err != nil {
		t.Fatal(err)
	}
	// The API generator treats $HOMEBREW_PREFIX and $APPDIR as relative, doubling absolute targets.
	payload := strings.NewReplacer(`"$HOMEBREW_PREFIX/bin/$`, `"$`, `"/Applications/$`, `"$`).Replace(jws.Payload)
	var casks []map[string]any
	if err := json.Unmarshal([]byte(payload), &casks); err != nil {
		t.Fatal(err)
	}
	// zeal's API entry has a Ruby object's inspect string for depends_on linux.
	known := map[string]bool{"zeal": true}
	var matched, total int
	for _, want := range casks {
		path, _ := want["ruby_source_path"].(string)
		src, err := os.ReadFile(filepath.Join(dir, path))
		if err != nil {
			continue
		}
		total++
		diffs := compareWithBrew(t, src, want)
		token := want["token"].(string)
		if len(diffs) == 0 {
			matched++
		} else if !known[token] {
			t.Errorf("%s:\n%s", token, strings.Join(diffs[:min(3, len(diffs))], "\n"))
		}
	}
	t.Logf("%d of %d casks match the API on every platform", matched, total)
	if total == 0 {
		t.Fatal("no casks found; is BRW_CASKRB_CASKS a homebrew-cask checkout?")
	}
}
