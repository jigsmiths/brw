package rb

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func serviceDetails(t *testing.T, service string) ServiceDetails {
	t.Helper()
	asBrewHost(t, "brewuser")
	oldEnv := lookupEnv
	lookupEnv = func(k string) (string, bool) { return "/Users/me", k == "HOME" }
	t.Cleanup(func() { lookupEnv = oldEnv })
	src := "class Tool < Formula\n  url \"https://example.com/tool-1.0.tar.gz\"\n  sha256 \"aa\"\n\n" + service + "end\n"
	f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.ServiceDetails("/opt/homebrew")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestServiceDetails(t *testing.T) {
	d := serviceDetails(t, `  service do
    run [opt_bin/"tool", "serve", "~/x"]
    keep_alive true
    working_dir var
    log_path "~/log/tool.log"
    error_log_path var/"log/tool.err"
    environment_variables FOO: "1", PATH: "/bin"
    run_type :interval
    interval 300
  end
`)
	want := ServiceDetails{
		Defined:   true,
		Command:   []string{"/opt/homebrew/opt/tool/bin/tool", "serve", "/Users/me/x"},
		Env:       [][2]string{{"FOO", "1"}, {"PATH", "/bin"}},
		KeepAlive: true,
		Timed:     true,
		Fields: map[string]string{
			"working_dir": "/opt/homebrew/var", "log_path": "~/log/tool.log",
			"error_log_path": "/opt/homebrew/var/log/tool.err", "interval": "300",
		},
	}
	if !reflect.DeepEqual(d, want) {
		t.Errorf("details = %+v\nwant      %+v", d, want)
	}
}

func TestServiceDetailsCron(t *testing.T) {
	for cron, want := range map[string][][2]string{
		"@yearly":   {{"Month", "1"}, {"Day", "1"}, {"Weekday", "*"}, {"Hour", "0"}, {"Minute", "0"}},
		"5 4 * * 1": {{"Month", "*"}, {"Day", "*"}, {"Weekday", "1"}, {"Hour", "4"}, {"Minute", "5"}},
		"@annually": {{"Month", "1"}, {"Day", "1"}, {"Weekday", "*"}, {"Hour", "0"}, {"Minute", "0"}},
	} {
		d := serviceDetails(t, "  service do\n    run [\"x\"]\n    run_type :cron\n    cron \""+cron+"\"\n  end\n")
		if !d.Timed || !reflect.DeepEqual(d.Cron, want) {
			t.Errorf("%s: timed %v, cron %v, want %v", cron, d.Timed, d.Cron, want)
		}
	}
}

func withHome(t *testing.T) {
	t.Helper()
	asBrewHost(t, "brewuser")
	oldEnv, oldHome := lookupEnv, lookupHome
	lookupEnv = func(k string) (string, bool) { return "/Users/me", k == "HOME" }
	lookupHome = func(name string) (string, bool) { return "/var/" + name, name == "root" }
	t.Cleanup(func() { lookupEnv, lookupHome = oldEnv, oldHome })
}

func TestServiceExpandsPathsLikeRuby(t *testing.T) {
	withHome(t)
	t.Chdir(t.TempDir())
	wd, _ := os.Getwd()
	src := "class Tool < Formula\n  url \"https://example.com/tool-1.0.tar.gz\"\n  sha256 \"aa\"\n\n  service do\n" +
		"    run [\"~\", \"~/a/../b\", \"~root/z\", \"./keep/../me\"]\n" +
		"    log_path \"rel/x.log\"\n    working_dir \"~/w//\"\n" +
		"    environment_variables HOME_DIR: \"~/x\"\n    keep_alive path: \"~/flag\"\n  end\nend\n"
	f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.ServiceFiles("/opt/homebrew")
	if err != nil {
		t.Fatal(err)
	}
	plist := files["sh.brew.tool.plist"]
	for _, want := range []string{
		"<string>/Users/me</string>\n\t\t<string>/Users/me/b</string>\n\t\t<string>/var/root/z</string>\n\t\t<string>./keep/../me</string>",
		"<key>StandardOutPath</key>\n\t<string>" + wd + "/rel/x.log</string>",
		"<key>WorkingDirectory</key>\n\t<string>/Users/me/w</string>",
		"<key>HOME_DIR</key>\n\t\t<string>~/x</string>",
		"<key>PathState</key>\n\t\t<string>~/flag</string>",
	} {
		if !strings.Contains(plist, want) {
			t.Errorf("plist lacks %q:\n%s", want, plist)
		}
	}
	bad := strings.Replace(src, "~root/z", "~nosuch/z", 1)
	f, err = LoadFormula([]byte(bad), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.ServiceFiles("/opt/homebrew"); err == nil || !strings.Contains(err.Error(), "user nosuch doesn't exist") {
		t.Errorf("err = %v", err)
	}
}

func TestServiceFromAPI(t *testing.T) {
	withHome(t)
	raw := `{"run":["$HOMEBREW_PREFIX/opt/tool/bin/tool","serve"],"run_type":"cron","cron":"@daily","keep_alive":{"always":true},
		"require_root":true,"working_dir":"$HOMEBREW_PREFIX/var","log_path":"~/log","interval":60,
		"environment_variables":{"B":"2","A":"$HOMEBREW_PREFIX/x"},"name":{"macos":"custom.tool"}}`
	got, err := ServiceFromAPI([]byte(raw), "/opt/homebrew")
	if err != nil {
		t.Fatal(err)
	}
	want := ServiceDetails{
		Defined: true, Command: []string{"/opt/homebrew/opt/tool/bin/tool", "serve"},
		Env:       [][2]string{{"B", "2"}, {"A", "/opt/homebrew/x"}},
		Fields:    map[string]string{"working_dir": "/opt/homebrew/var", "log_path": "~/log", "interval": "60"},
		Cron:      [][2]string{{"Month", "*"}, {"Day", "*"}, {"Weekday", "*"}, {"Hour", "0"}, {"Minute", "0"}},
		KeepAlive: true, Timed: true, RequiresRoot: true, MacName: "custom.tool",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("details = %+v\nwant      %+v", got, want)
	}
	for _, none := range []string{"null", ""} {
		if got, err := ServiceFromAPI([]byte(none), "/opt/homebrew"); err != nil || got.Defined {
			t.Errorf("%q: %+v, %v", none, got, err)
		}
	}
	got, _ = ServiceFromAPI([]byte(`{"run":{"macos":["/m"],"linux":["/l"]},"keep_alive":{"always":false}}`), "/opt/homebrew")
	if !reflect.DeepEqual(got.Command, []string{"/m"}) || got.KeepAlive {
		t.Errorf("details = %+v", got)
	}
}

func TestServiceDetailsRequiresRootAndName(t *testing.T) {
	d := serviceDetails(t, "  service do\n    run [\"x\"]\n    require_root true\n    name macos: \"mac.tool\", linux: \"lin.tool\"\n  end\n")
	if !d.RequiresRoot || d.MacName != "mac.tool" {
		t.Errorf("details = %+v", d)
	}
}

func TestServiceDetailsWithoutABlock(t *testing.T) {
	if d := serviceDetails(t, ""); d.Defined || len(d.Command) != 0 {
		t.Errorf("details = %+v", d)
	}
	if d := serviceDetails(t, "  service do\n    name macos: \"x\"\n  end\n"); !d.Defined || len(d.Command) != 0 {
		t.Errorf("details = %+v", d)
	}
}
