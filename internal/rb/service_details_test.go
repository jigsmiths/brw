package rb

import (
	"reflect"
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

func TestServiceDetailsWithoutABlock(t *testing.T) {
	if d := serviceDetails(t, ""); d.Defined || len(d.Command) != 0 {
		t.Errorf("details = %+v", d)
	}
	if d := serviceDetails(t, "  service do\n    name macos: \"x\"\n  end\n"); !d.Defined || len(d.Command) != 0 {
		t.Errorf("details = %+v", d)
	}
}
