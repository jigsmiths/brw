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
		KeepAlive: true, Timed: true, RequiresRoot: true, NamesMac: true, MacName: "custom.tool",
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
	if !d.RequiresRoot || !d.NamesMac || d.MacName != "mac.tool" {
		t.Errorf("details = %+v", d)
	}
}

func TestServiceCronFieldsFollowRubysInteger(t *testing.T) {
	d := serviceDetails(t, "  service do\n    run [\"x\"]\n    run_type :cron\n    cron \"0x1f 017 1_0 0b11 +5\"\n  end\n")
	want := [][2]string{{"Month", "3"}, {"Day", "10"}, {"Weekday", "5"}, {"Hour", "15"}, {"Minute", "31"}}
	if !reflect.DeepEqual(d.Cron, want) {
		t.Errorf("cron = %v, want %v", d.Cron, want)
	}
}

func TestServiceCronAcceptsHugeIntegersLikeRuby(t *testing.T) {
	got := serviceFiles(t, "  service do\n    run [\"x\"]\n    run_type :cron\n    cron \"99999999999999999999 0x7fffffffffffffffff 1_0 -3 123456789012345678901234567890\"\n  end\n")
	for _, want := range []string{
		"\t\t<key>Day</key>\n\t\t<integer>10</integer>\n",
		"\t\t<key>Hour</key>\n\t\t<integer>2361183241434822606847</integer>\n",
		"\t\t<key>Minute</key>\n\t\t<integer>99999999999999999999</integer>\n",
		"\t\t<key>Month</key>\n\t\t<integer>-3</integer>\n",
		"\t\t<key>Weekday</key>\n\t\t<integer>123456789012345678901234567890</integer>\n",
	} {
		if !strings.Contains(got["sh.brew.tool.plist"], want) {
			t.Errorf("plist lacks %q:\n%s", want, got["sh.brew.tool.plist"])
		}
	}
	if want := "OnCalendar=*--3-10 2361183241434822606847:99999999999999999999:00\n"; !strings.Contains(got["sh.brew.tool.timer"], want) {
		t.Errorf("timer:\n%s", got["sh.brew.tool.timer"])
	}
}

func TestServiceCronErrorsAreBrews(t *testing.T) {
	withHome(t)
	for cron, want := range map[string]string{
		"5 4 * * x":    `invalid value for Integer(): "x"`,
		"08 * * * *":   `invalid value for Integer(): "08"`,
		"1__0 * * * *": `invalid value for Integer(): "1__0"`,
		"1 2 3 4":      "Service#parse_cron expects a valid cron syntax",
		"@reboot":      "Service#parse_cron expects a valid cron syntax",
	} {
		src := "class Tool < Formula\n  url \"https://example.com/tool-1.0.tar.gz\"\n  sha256 \"aa\"\n\n  service do\n" +
			"    run [\"x\"]\n    run_type :cron\n    cron \"" + cron + "\"\n  end\nend\n"
		f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
		if err == nil {
			_, err = f.ServiceDetails("/opt/homebrew")
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", cron, err, want)
		}
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
