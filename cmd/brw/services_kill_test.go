package main

import (
	"strings"
	"testing"
)

const keepAliveFormula = `class Foo < Formula
  desc "Foo"
  homepage "https://example.com"
  url "https://example.com/foo-1.0.tar.gz"
  sha256 "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

  service do
    run [opt_bin/"foo", "serve"]
    keep_alive true
  end
end
`

func TestServicesKill(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	out, err := runCmd(t, e, "services", "kill", "-prefix", prefix, "foo")
	if want := "Service `foo` is not started.\n"; err != nil || out != want {
		t.Errorf("not started: out %q, err %v", out, err)
	}

	lc.loaded["sh.brew.foo"] = runningOut
	lc.stopKills = true
	out, err = runCmd(t, e, "services", "kill", "-prefix", prefix, "foo")
	want := "Killing `foo`... (might take a while)\n==> Successfully killed `foo` (label: sh.brew.foo)\n"
	if err != nil || out != want || calls(lc) != "stop sh.brew.foo" {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
}

func TestServicesKillKillsTheLabelThatIsLoaded(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["homebrew.mxcl.foo"] = runningOut
	lc.stopKills = true
	out, err := runCmd(t, e, "services", "kill", "-prefix", prefix, "foo")
	if err != nil || !strings.Contains(out, "(label: homebrew.mxcl.foo)") || calls(lc) != "stop homebrew.mxcl.foo" {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
}

func TestServicesKillWarnsWhenTheServiceSurvives(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "kill", "-prefix", prefix, "foo")
	if err != nil || out != "Killing `foo`... (might take a while)\n" {
		t.Errorf("out %q, err %v", out, err)
	}
	if want := "Warning: Unable to kill `foo` (label: sh.brew.foo)\n"; stderr(e) != want {
		t.Errorf("stderr = %q, want %q", stderr(e), want)
	}
}

func TestServicesKillRefusesAServiceThatKeepsItselfAlive(t *testing.T) {
	e, prefix, _, lc := servicesEnvFiles(t, file(".brew/foo.rb", keepAliveFormula, 0o644))
	lc.loaded["sh.brew.foo"] = runningOut
	out, err := runCmd(t, e, "services", "kill", "-prefix", prefix, "foo")
	want := "Service `foo` is set to automatically restart and can't be killed.\n"
	if err != nil || out != want || len(lc.calls) != 0 {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
}

func TestServicesKillUsesTheAPIsServiceBlockOfAFormulaThatIsNotInstalled(t *testing.T) {
	e, prefix, _, lc := servicesEnv(t)
	lc.loaded["sh.brew.daemon"] = runningOut
	out, err := runCmd(t, e, "services", "kill", "-prefix", prefix, "daemon")
	want := "Service `daemon` is set to automatically restart and can't be killed.\n"
	if err != nil || out != want || len(lc.calls) != 0 {
		t.Errorf("out %q, err %v, calls %q", out, err, lc.calls)
	}
}

func TestServicesInfoUsesTheAPIsServiceBlockOfAFormulaThatIsNotInstalled(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	out, err := runCmd(t, e, "services", "info", "-v", "-prefix", prefix, "daemon")
	want := "daemon (sh.brew.daemon)\nRunning: false\nLoaded: false\nSchedulable: false\n" +
		"File: " + prefix + "/opt/daemon/sh.brew.daemon.plist true\nRegistered at login: false\n" +
		"Command: " + prefix + "/opt/daemon/bin/daemon serve\nWorking directory: " + prefix + "/var\n"
	if err != nil || out != want {
		t.Errorf("out:\n%s\nwant:\n%s\n%v", out, want, err)
	}
}

func TestServicesKillAll(t *testing.T) {
	e, prefix, _, _ := servicesEnv(t)
	out, err := runCmd(t, e, "services", "kill", "--all", "-prefix", prefix)
	if want := "Service `foo` is not started.\n"; err != nil || out != want {
		t.Errorf("out %q, err %v", out, err)
	}
}
