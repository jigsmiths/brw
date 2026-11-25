package services

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseRealPrint(t *testing.T) {
	got := ParsePrint(fixture(t, "print_rift.txt"))
	if got != (Info{ExitCode: 78, HasExitCode: true}) {
		t.Errorf("print: %+v", got)
	}
	if got := ParsePrint("\tstate = running\n\tpid = 42\n"); got.PID != 42 || !got.Running() || got.HasExitCode {
		t.Errorf("running: %+v", got)
	}
	if got := ParsePrint("\tstate = not running\n\tlast exit code = (never exited)\n"); got.HasExitCode {
		t.Errorf("never exited has no code: %+v", got)
	}
}

func TestParseRealList(t *testing.T) {
	got := ParseList(fixture(t, "list_rift.txt"))
	if got != (Info{ExitCode: 19968, HasExitCode: true}) {
		t.Errorf("list: %+v", got)
	}
	if got := ParseList("{\n\t\"PID\" = 7;\n\t\"LastExitStatus\" = 0;\n};\n"); got.PID != 7 || !got.HasExitCode {
		t.Errorf("list with pid: %+v", got)
	}
}

func TestClassifyFollowsBrewsOrder(t *testing.T) {
	cases := []struct {
		name string
		in   State
		want string
	}{
		{"started wins", State{Loaded: true, Info: Info{PID: 1, ExitCode: 3, HasExitCode: true}}, "started"},
		{"not loaded", State{Info: Info{HasExitCode: true}}, "none"},
		{"scheduled", State{Loaded: true, Timed: true, Info: Info{HasExitCode: true}}, "scheduled"},
		{"stopped", State{Loaded: true, Info: Info{HasExitCode: true}}, "stopped"},
		{"error", State{Loaded: true, Info: Info{ExitCode: 78, HasExitCode: true}}, "error"},
		{"unknown", State{Loaded: true, Info: Info{Blank: true}}, "unknown"},
		{"loaded but never exited", State{Loaded: true}, "other"},
	}
	for _, c := range cases {
		if got := Classify(c.in); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func TestFindPlistPrefersCanonical(t *testing.T) {
	keg := t.TempDir()
	names := Names("foo")
	if _, err := FindPlist(keg, names); !errors.Is(err, ErrNoService) {
		t.Errorf("empty keg: %v", err)
	}
	for _, n := range []string{"homebrew.mxcl.foo.plist", "sh.brew.foo.plist"} {
		if err := os.WriteFile(filepath.Join(keg, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := FindPlist(keg, names); err != nil || filepath.Base(got) != "sh.brew.foo.plist" {
		t.Errorf("FindPlist = %q, %v", got, err)
	}
}

func TestFindPlistFollowsAnExplicitName(t *testing.T) {
	keg := t.TempDir()
	for _, n := range []string{"zz.custom.plist", "sh.brew.foo.plist", "a.plist"} {
		if err := os.WriteFile(filepath.Join(keg, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := FindPlist(keg, []string{"zz.custom"}); err != nil || filepath.Base(got) != "zz.custom.plist" {
		t.Errorf("explicit name: %q, %v", got, err)
	}
	pkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(pkg, "vendor.plist"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := FindPlist(pkg, Names("foo")); err != nil || filepath.Base(got) != "vendor.plist" {
		t.Errorf("a package plist is still a service: %q, %v", got, err)
	}
}

func TestParseEnvFileIsBrews(t *testing.T) {
	env, skipped := ParseEnvFile("# comment\n\n  FOO = bar baz \nURL=http://x/?a=b\nnokey\n=novalue\nEMPTY=\n")
	want := map[string]string{"FOO": "bar baz", "URL": "http://x/?a=b", "EMPTY": ""}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("env = %v, want %v", env, want)
	}
	if !reflect.DeepEqual(skipped, []string{"nokey", "=novalue"}) {
		t.Errorf("skipped = %q", skipped)
	}
}

const envPlistHead = "<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<plist version=\"1.0\">\n<dict>\n"

func TestApplyEnvMergesIntoTheExistingDictSorted(t *testing.T) {
	in := envPlistHead +
		"\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>NOTE</key>\n\t\t<string>a &amp; b</string>\n\t\t<key>PATH</key>\n\t\t<string>/bin</string>\n\t</dict>\n" +
		"\t<key>Label</key>\n\t<string>sh.brew.foo</string>\n</dict>\n</plist>\n"
	got, ok := ApplyEnv([]byte(in), map[string]string{"PATH": "/x:/y", "ALPHA": "1 < 2"})
	want := envPlistHead +
		"\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>ALPHA</key>\n\t\t<string>1 &lt; 2</string>\n\t\t<key>NOTE</key>\n\t\t<string>a &amp; b</string>\n" +
		"\t\t<key>PATH</key>\n\t\t<string>/x:/y</string>\n\t</dict>\n" +
		"\t<key>Label</key>\n\t<string>sh.brew.foo</string>\n</dict>\n</plist>\n"
	if !ok || string(got) != want {
		t.Errorf("ApplyEnv = %v\n%s\nwant\n%s", ok, got, want)
	}
}

func TestApplyEnvAddsTheDictInKeyOrder(t *testing.T) {
	in := envPlistHead + "\t<key>Disabled</key>\n\t<false/>\n\t<key>KeepAlive</key>\n\t<true/>\n\t<key>Label</key>\n\t<string>sh.brew.foo</string>\n</dict>\n</plist>\n"
	got, ok := ApplyEnv([]byte(in), map[string]string{"B": "2", "A": "1"})
	want := envPlistHead + "\t<key>Disabled</key>\n\t<false/>\n" +
		"\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>A</key>\n\t\t<string>1</string>\n\t\t<key>B</key>\n\t\t<string>2</string>\n\t</dict>\n" +
		"\t<key>KeepAlive</key>\n\t<true/>\n\t<key>Label</key>\n\t<string>sh.brew.foo</string>\n</dict>\n</plist>\n"
	if !ok || string(got) != want {
		t.Errorf("ApplyEnv = %v\n%s\nwant\n%s", ok, got, want)
	}
}

func TestApplyEnvAppendsWhenItSortsLast(t *testing.T) {
	in := envPlistHead + "\t<key>Abc</key>\n\t<true/>\n</dict>\n</plist>\n"
	got, ok := ApplyEnv([]byte(in), map[string]string{"A": "1"})
	if !ok || !strings.HasSuffix(string(got), "\t<key>Abc</key>\n\t<true/>\n\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>A</key>\n\t\t<string>1</string>\n\t</dict>\n</dict>\n</plist>\n") {
		t.Errorf("ApplyEnv = %v\n%s", ok, got)
	}
}

func TestApplyEnvLeavesAnUneditablePlistAlone(t *testing.T) {
	for _, in := range []string{"bplist00\x00", "<plist>no dict</plist>"} {
		if got, ok := ApplyEnv([]byte(in), map[string]string{"A": "1"}); ok || string(got) != in {
			t.Errorf("ApplyEnv(%q) = %q, %v", in, got, ok)
		}
	}
	if got, ok := ApplyEnv([]byte(envPlistHead+"</dict>\n</plist>\n"), nil); !ok || string(got) != envPlistHead+"</dict>\n</plist>\n" {
		t.Errorf("no overrides must not change the plist: %q, %v", got, ok)
	}
}

func TestPlistKeys(t *testing.T) {
	body := []byte("<dict>\n<key>Label</key>\n<string>homebrew.mxcl.foo</string>\n<key>UserName</key><string>bob</string></dict>")
	if l, ok := FileLabel(body); !ok || l != "homebrew.mxcl.foo" {
		t.Errorf("label = %q, %v", l, ok)
	}
	if u, ok := FileUser(body); !ok || u != "bob" {
		t.Errorf("user = %q, %v", u, ok)
	}
}

func TestFormatTableMatchesBrewNoColor(t *testing.T) {
	var rows []Row
	for _, n := range []string{"atuin", "cloudflared", "emacs", "zellij"} {
		rows = append(rows, Row{Name: n, Status: "none"})
	}
	if got, want := FormatTable(rows, false, "/home/me"), fixture(t, "brew_list_none.txt"); got != want {
		t.Errorf("table:\n%q\nwant\n%q", got, want)
	}
}

func TestFormatTableColorAndColumns(t *testing.T) {
	user, code := "me", 78
	rows := []Row{
		{Name: "foo", Status: "started", User: &user, File: "/home/me/x.plist", Loaded: true},
		{Name: "bar", Status: "error", User: &user, File: "/home/me/y.plist", Loaded: true, Exit: &code},
		{Name: "baz", Status: "none", File: "/opt/baz.plist"},
	}
	got := FormatTable(rows, true, "/home/me")
	want := "\x1b[1mName Status    User File\x1b[0m\n" +
		"foo  \x1b[32mstarted\x1b[0m   me   ~/x.plist\n" +
		"bar  \x1b[31merror  \x1b[0m78 me   ~/y.plist\n" +
		"baz  \x1b[39mnone\x1b[0m           \n"
	if got != want {
		t.Errorf("table:\n%q\nwant\n%q", got, want)
	}
	plain := FormatTable(rows[1:2], false, "/home/me")
	if !strings.Contains(plain, "error  78") {
		t.Errorf("error status shows the exit code: %q", plain)
	}
}

func TestFormatJSONMatchesBrew(t *testing.T) {
	var rows []Row
	for _, n := range []string{"atuin", "cloudflared", "emacs", "zellij"} {
		rows = append(rows, Row{Name: n, Status: "none"})
	}
	rows[0].File = "/opt/homebrew/opt/atuin/sh.brew.atuin.plist"
	rows[1].File = "/opt/homebrew/opt/cloudflared/sh.brew.cloudflared.plist"
	rows[2].File = "/opt/homebrew/opt/emacs/homebrew.mxcl.emacs.plist"
	rows[3].File = "/opt/homebrew/opt/zellij/sh.brew.zellij.plist"
	got, err := FormatJSON(rows)
	if err != nil || got != fixture(t, "brew_list_none.json") {
		t.Errorf("json:\n%s\nwant\n%s (%v)", got, fixture(t, "brew_list_none.json"), err)
	}
	if got, _ := FormatJSON(nil); got != "[]\n" {
		t.Errorf("empty = %q", got)
	}
}

type probeLaunchctl struct {
	Launchctl
	calls    []string
	printOut map[string]string
	listOut  string
}

func (p *probeLaunchctl) Print(domain, label string) (string, error) {
	p.calls = append(p.calls, "print "+domain+"/"+label)
	if out, ok := p.printOut[domain]; ok {
		return out, nil
	}
	return "", ErrNotLoaded
}

func (p *probeLaunchctl) List(label string) (string, error) {
	p.calls = append(p.calls, "list "+label)
	if p.listOut == "" {
		return "", ErrNotLoaded
	}
	return p.listOut, nil
}

func TestQueryProbesEachDomainThenList(t *testing.T) {
	p := &probeLaunchctl{listOut: fixture(t, "list_rift.txt")}
	loaded, info, err := Query(p, Domains(501), "homebrew.mxcl.rift")
	if err != nil || !loaded || info.ExitCode != 19968 {
		t.Fatalf("Query = %v, %+v, %v", loaded, info, err)
	}
	want := "print gui/501/homebrew.mxcl.rift,print user/501/homebrew.mxcl.rift,list homebrew.mxcl.rift"
	if got := strings.Join(p.calls, ","); got != want {
		t.Errorf("probe order:\n%s\nwant\n%s", got, want)
	}
}

func TestQueryStopsAtTheFirstDomainThatAnswers(t *testing.T) {
	p := &probeLaunchctl{printOut: map[string]string{"user/501": fixture(t, "print_rift.txt")}}
	loaded, info, err := Query(p, Domains(501), "homebrew.mxcl.rift")
	if err != nil || !loaded || info.ExitCode != 78 {
		t.Fatalf("Query = %v, %+v, %v", loaded, info, err)
	}
	if got := strings.Join(p.calls, ","); got != "print gui/501/homebrew.mxcl.rift,print user/501/homebrew.mxcl.rift" {
		t.Errorf("calls = %s", got)
	}
}

func TestQueryTreatsEmptyOutputAsNotLoaded(t *testing.T) {
	p := &probeLaunchctl{printOut: map[string]string{"gui/501": "  \n"}}
	if loaded, _, _ := Query(p, Domains(501), "x"); loaded {
		t.Error("blank print output is not a loaded service")
	}
}
