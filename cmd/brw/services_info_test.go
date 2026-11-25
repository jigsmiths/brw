package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const infoFormula = `class Foo < Formula
  desc "Foo"
  homepage "https://example.com"
  url "https://example.com/foo-1.0.tar.gz"
  sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

  service do
    run [opt_bin/"foo", "serve"]
    working_dir var
    log_path var/"log/foo.log"
    error_log_path var/"log/foo.err"
    environment_variables FROM_FORMULA: "1", PATH: "/bin"
    run_type :cron
    cron "5 4 * * 1"
    interval 300
  end
end
`

func TestServicesInfo(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	out, err := runCmd(t, e, "services", "info", "-prefix", prefix, "foo")
	want := "foo (sh.brew.foo)\nRunning: false\nLoaded: false\nSchedulable: false\n"
	if err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}

	writeFile(t, filepath.Join(agents, "sh.brew.foo.plist"), testPlist)
	lc.loaded["sh.brew.foo"] = runningOut
	out, err = runCmd(t, e, "services", "i", "-prefix", prefix, "foo")
	want = "foo (sh.brew.foo)\nRunning: true\nLoaded: true\nSchedulable: false\nUser: me\nPID: 42\n"
	if err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}
}

func TestServicesInfoColorsOnATerminal(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	e.tty = true
	out, err := runCmd(t, e, "services", "info", "-prefix", prefix, "foo")
	no := "\x1b[1m\x1b[31m✘\x1b[0m\x1b[0m"
	want := "\x1b[1mfoo\x1b[0m (sh.brew.foo)\nRunning: " + no + "\nLoaded: " + no + "\nSchedulable: " + no + "\n"
	if err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}

	t.Setenv("HOMEBREW_NO_EMOJI", "1")
	out, _ = runCmd(t, e, "services", "info", "-prefix", prefix, "foo")
	if !strings.Contains(out, "Running: false\n") {
		t.Errorf("no emoji: %q", out)
	}
}

func TestServicesInfoWithAServiceBlockAndTheVerboseFlag(t *testing.T) {
	e, prefix, _, _ := servicesEnvFiles(t, file(".brew/foo.rb", infoFormula, 0o644), file("sh.brew.foo.plist", testPlist, 0o644))
	writeEnvFile(t, e, 0o600, "FOO=bar\nFROM_FORMULA=2\n")
	out, err := runCmd(t, e, "services", "info", "-v", "-prefix", prefix, "foo")
	want := "foo (sh.brew.foo)\nRunning: false\nLoaded: false\nSchedulable: true\n" +
		"File: " + prefix + "/opt/foo/sh.brew.foo.plist true\n" +
		"Registered at login: false\n" +
		"Command: FROM_FORMULA=\"2\" FOO=\"bar\" " + prefix + "/opt/foo/bin/foo serve\n" +
		"Working directory: " + prefix + "/var\n" +
		"Log: " + prefix + "/var/log/foo.log\n" +
		"Error log: " + prefix + "/var/log/foo.err\n" +
		"Interval: 300s\n" +
		"Cron: {Month: \"*\", Day: \"*\", Weekday: 1, Hour: 4, Minute: 5}\n"
	if err != nil || out != want {
		t.Errorf("out:\n%s\nwant:\n%s\n%v", out, want, err)
	}
}

func TestServicesInfoQuotesTheCommandLikeAShell(t *testing.T) {
	formula := strings.Replace(infoFormula, `"serve"]`, `"two words", "it's", "a&b"]`, 1)
	e, prefix, _, _ := servicesEnvFiles(t, file(".brew/foo.rb", formula, 0o644), file("sh.brew.foo.plist", testPlist, 0o644))
	out, err := runCmd(t, e, "services", "info", "-v", "-prefix", prefix, "foo")
	want := "Command: FROM_FORMULA=\"1\" " + prefix + "/opt/foo/bin/foo two\\ words it\\'s a\\&b\n"
	if err != nil || !strings.Contains(out, want) {
		t.Errorf("out:\n%s\nwant a line %q (%v)", out, want, err)
	}
}

func TestServicesInfoVerboseWithoutAServiceBlock(t *testing.T) {
	e, prefix, agents, _ := servicesEnv(t)
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	writeFile(t, dst, testPlist)
	out, err := runCmd(t, e, "services", "info", "--verbose", "-prefix", prefix, "foo")
	want := "foo (sh.brew.foo)\nRunning: false\nLoaded: false\nSchedulable: false\n" +
		"File: " + dst + " true\nRegistered at login: true\n"
	if err != nil || out != want {
		t.Errorf("out %q, err %v, want %q", out, err, want)
	}
}

func TestServicesInfoSchedulableComesFromTheServiceBlockNotThePlist(t *testing.T) {
	timed := "<plist><dict><key>Label</key><string>sh.brew.foo</string><key>StartInterval</key><integer>60</integer></dict></plist>\n"
	e, prefix, _, _ := servicesEnvFiles(t, file("sh.brew.foo.plist", timed, 0o644))
	out, err := runCmd(t, e, "services", "info", "-prefix", prefix, "foo")
	if err != nil || !strings.Contains(out, "Schedulable: false\n") {
		t.Errorf("out %q, err %v", out, err)
	}
}

func TestServicesInfoForAFormulaWithoutAService(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	for _, name := range []string{"plain", "ghost"} {
		out, err := runCmd(t, e, "services", "info", "-v", "-prefix", prefix, name)
		want := name + " (sh.brew." + name + ")\nRunning: false\nLoaded: false\nSchedulable: false\n" +
			"File: " + prefix + "/opt/" + name + "/sh.brew." + name + ".plist true\nRegistered at login: false\n"
		if err != nil || out != want {
			t.Errorf("%s: out %q, err %v, want %q", name, out, err, want)
		}
	}
}

func TestServicesInfoJSON(t *testing.T) {
	e, prefix, agents, lc := servicesEnv(t)
	dst := filepath.Join(agents, "sh.brew.foo.plist")
	writeFile(t, dst, testPlist)
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "info", "--json", "-prefix", prefix, "foo")
	want := "[\n  {\n    \"name\": \"foo\",\n    \"service_name\": \"sh.brew.foo\",\n    \"running\": true,\n" +
		"    \"loaded\": true,\n    \"schedulable\": false,\n    \"pid\": 42,\n    \"exit_code\": null,\n" +
		"    \"user\": \"me\",\n    \"status\": \"started\",\n    \"file\": \"" + dst + "\",\n" +
		"    \"registered\": true,\n    \"loaded_file\": null\n  }\n]\n"
	if err != nil || out != want {
		t.Errorf("out:\n%s\nwant:\n%s\n%v", out, want, err)
	}
}

func TestServicesInfoJSONReportsTheLoadedFileAndExitCode(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	real, err := os.ReadFile("../../internal/services/testdata/print_rift.txt")
	if err != nil {
		t.Fatal(err)
	}
	lc.loaded["sh.brew.foo"] = string(real)
	out, err := runCmd(t, e, "services", "info", "--json", "-prefix", prefix, "foo")
	for _, want := range []string{"\"exit_code\": 78,", "\"status\": \"error\"", "\"pid\": null,",
		"\"loaded_file\": \"/Users/me/Library/LaunchAgents/homebrew.mxcl.rift.plist\"", "\"user\": null,"} {
		if err != nil || !strings.Contains(out, want) {
			t.Errorf("out lacks %q:\n%s (%v)", want, out, err)
		}
	}
}

func TestServicesInfoJSONWithAServiceBlock(t *testing.T) {
	e, prefix, _, _ := servicesEnvFiles(t, file(".brew/foo.rb", infoFormula, 0o644), file("sh.brew.foo.plist", testPlist, 0o644))
	out, err := runCmd(t, e, "services", "info", "--json", "-prefix", prefix, "foo")
	want := "    \"registered\": false,\n    \"loaded_file\": null,\n" +
		"    \"command\": \"FROM_FORMULA=\\\"1\\\" " + prefix + "/opt/foo/bin/foo serve\",\n" +
		"    \"working_dir\": \"" + prefix + "/var\",\n    \"root_dir\": null,\n" +
		"    \"log_path\": \"" + prefix + "/var/log/foo.log\",\n    \"error_log_path\": \"" + prefix + "/var/log/foo.err\",\n" +
		"    \"interval\": 300,\n    \"cron\": {\n      \"Month\": \"*\",\n      \"Day\": \"*\",\n      \"Weekday\": 1,\n" +
		"      \"Hour\": 4,\n      \"Minute\": 5\n    }\n  }\n]\n"
	if err != nil || !strings.HasSuffix(out, want) || !strings.Contains(out, "\"schedulable\": true,") {
		t.Errorf("out:\n%s\nwant suffix:\n%s\n%v", out, want, err)
	}
}

func TestServicesInfoAllListsEveryService(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	out, err := runCmd(t, e, "services", "info", "--all", "--json", "-prefix", prefix)
	if err != nil || strings.Count(out, "\"name\":") != 1 || !strings.Contains(out, "\"name\": \"foo\"") {
		t.Errorf("out %q, err %v", out, err)
	}
	out, err = runCmd(t, e, "services", "info", "-prefix", prefix, "foo", "plain")
	if err != nil || !strings.Contains(out, "foo (sh.brew.foo)\n") || !strings.Contains(out, "plain (sh.brew.plain)\n") {
		t.Errorf("out %q, err %v", out, err)
	}
}
