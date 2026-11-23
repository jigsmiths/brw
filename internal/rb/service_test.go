package rb

import (
	"maps"
	"reflect"
	"slices"
	"testing"
)

const plistHead = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
`

const sessionTypes = "\t<key>LimitLoadToSessionType</key>\n\t<array>\n\t\t<string>Aqua</string>\n\t\t<string>Background</string>\n" +
	"\t\t<string>LoginWindow</string>\n\t\t<string>StandardIO</string>\n\t\t<string>System</string>\n\t</array>\n"

func keys(m map[string]string) []string { return slices.Sorted(maps.Keys(m)) }

func serviceFiles(t *testing.T, service string) map[string]string {
	t.Helper()
	asBrewHost(t, "brewuser")
	t.Setenv("HOME", "/Users/me")
	src := "class Tool < Formula\n  url \"https://example.com/tool-1.0.tar.gz\"\n  sha256 \"aa\"\n\n" + service + "end\n"
	f, err := LoadFormula([]byte(src), "tool", "arm64_golden_gate")
	if err != nil {
		t.Fatal(err)
	}
	files, err := f.ServiceFiles("/opt/homebrew")
	if err != nil {
		t.Fatal(err)
	}
	return files
}

// As brew's install_service writes them after linking, so paths in the keg
// are opt paths.
func TestServiceFilesAreBrews(t *testing.T) {
	got := serviceFiles(t, `  service do
    run [bin/"tool", "server", "-dev"]
    keep_alive successful_exit: false
    working_dir var
    log_path var/"log/tool.log"
    error_log_path var/"log/tool.log"
    environment_variables PATH: std_service_path_env, NOTE: "a & b"
  end
`)
	want := map[string]string{
		"sh.brew.tool.plist": plistHead + "<dict>\n" +
			"\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>NOTE</key>\n\t\t<string>a &amp; b</string>\n" +
			"\t\t<key>PATH</key>\n\t\t<string>/opt/homebrew/bin:/opt/homebrew/sbin:/usr/bin:/bin:/usr/sbin:/sbin</string>\n\t</dict>\n" +
			"\t<key>KeepAlive</key>\n\t<dict>\n\t\t<key>SuccessfulExit</key>\n\t\t<false/>\n\t</dict>\n" +
			"\t<key>Label</key>\n\t<string>sh.brew.tool</string>\n" + sessionTypes +
			"\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>/opt/homebrew/opt/tool/bin/tool</string>\n" +
			"\t\t<string>server</string>\n\t\t<string>-dev</string>\n\t</array>\n" +
			"\t<key>RunAtLoad</key>\n\t<true/>\n" +
			"\t<key>StandardErrorPath</key>\n\t<string>/opt/homebrew/var/log/tool.log</string>\n" +
			"\t<key>StandardOutPath</key>\n\t<string>/opt/homebrew/var/log/tool.log</string>\n" +
			"\t<key>WorkingDirectory</key>\n\t<string>/opt/homebrew/var</string>\n" +
			"</dict>\n</plist>\n",
		"sh.brew.tool.service": "[Unit]\nDescription=Homebrew generated unit for tool\n\n[Install]\nWantedBy=default.target\n\n" +
			"[Service]\nType=simple\nExecStart=\"/opt/homebrew/opt/tool/bin/tool\" \"server\" \"-dev\"\nRestart=on-failure\n" +
			"WorkingDirectory=/opt/homebrew/var\nStandardOutput=append:/opt/homebrew/var/log/tool.log\n" +
			"StandardError=append:/opt/homebrew/var/log/tool.log\n" +
			"Environment=\"PATH=/opt/homebrew/bin:/opt/homebrew/sbin:/usr/bin:/bin:/usr/sbin:/sbin\"\nEnvironment=\"NOTE=a & b\"\n",
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s:\n%s\nwant:\n%s", name, got[name], w)
		}
	}
	if len(got) != len(want) {
		t.Errorf("files = %v", keys(got))
	}
}

func TestTimedServiceFiles(t *testing.T) {
	got := serviceFiles(t, `  service do
    run opt_bin/"tool"
    run_type :cron
    cron "5 * * * 1"
    run_at_load false
    launch_only_once true
    name macos: "com.example.tool", linux: "example-tool"
  end
`)
	plist := plistHead + "<dict>\n\t<key>Label</key>\n\t<string>com.example.tool</string>\n" +
		"\t<key>LaunchOnlyOnce</key>\n\t<true/>\n" + sessionTypes +
		"\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>/opt/homebrew/opt/tool/bin/tool</string>\n\t</array>\n" +
		"\t<key>RunAtLoad</key>\n\t<false/>\n" +
		"\t<key>StartCalendarInterval</key>\n\t<dict>\n\t\t<key>Minute</key>\n\t\t<integer>5</integer>\n" +
		"\t\t<key>Weekday</key>\n\t\t<integer>1</integer>\n\t</dict>\n</dict>\n</plist>\n"
	timer := "[Unit]\nDescription=Homebrew generated timer for tool\n\n[Install]\nWantedBy=timers.target\n\n" +
		"[Timer]\nUnit=example-tool.service\nPersistent=true\nOnCalendar=Mon *-*-* *:05:00\n"
	if got["com.example.tool.plist"] != plist {
		t.Errorf("plist:\n%s\nwant:\n%s", got["com.example.tool.plist"], plist)
	}
	if got["example-tool.timer"] != timer {
		t.Errorf("timer:\n%s\nwant:\n%s", got["example-tool.timer"], timer)
	}
	if want := []string{"com.example.tool.plist", "example-tool.service", "example-tool.timer"}; !reflect.DeepEqual(keys(got), want) {
		t.Errorf("files = %v, want %v", keys(got), want)
	}
}

func TestServiceWithoutRunHasNoFiles(t *testing.T) {
	if got := serviceFiles(t, "  service do\n    name macos: \"x\"\n  end\n"); len(got) != 0 {
		t.Errorf("files = %v", keys(got))
	}
	if got := serviceFiles(t, ""); len(got) != 0 {
		t.Errorf("files = %v", keys(got))
	}
}
