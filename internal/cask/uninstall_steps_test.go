package cask

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/sudo"
)

func uninstallStepsCask(log, app string) string {
	step := func(name, test string) string {
		return fmt.Sprintf(`{"steps":[{"type":"run","command":{"path":"/bin/sh"},"writable_paths":[{"path":%q}],"args":["-c","if [ %s -d %s ]; then echo %s >> %s; fi"]}]}`,
			filepath.Dir(log), test, app, name, log)
	}
	return `[{"app":["App.app"]},
		{"uninstall_preflight_steps":[` + step("preflight-app-present", "") + `]},
		{"uninstall_postflight_steps":[` + step("postflight-app-gone", "!") + `]}]`
}

func TestUninstallStepsRunAroundTheOtherArtifactsOnlyWhenUninstalling(t *testing.T) {
	l := newSudoLink(t)
	l.ti.in.Run, l.ti.in.Exec = nil, nil // the steps run for real, under the sandbox where macOS has one
	log := filepath.Join(t.TempDir(), "log")
	app := filepath.Join(l.ti.appDir, "App.app")
	if err := l.ti.in.Install(context.Background(), l.ti.cask(t, uninstallStepsCask(log, app))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("an uninstall step ran while installing")
	}
	receipt, err := os.ReadFile(filepath.Join(l.ti.prefix, "Caskroom/app/.metadata/INSTALL_RECEIPT.json"))
	if err != nil {
		t.Fatal(err)
	}
	var tab struct {
		UninstallArtifacts []map[string]json.RawMessage `json:"uninstall_artifacts"`
	}
	if err := json.Unmarshal(receipt, &tab); err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, a := range tab.UninstallArtifacts {
		for k, v := range a {
			kinds = append(kinds, k)
			if !strings.HasPrefix(k, "uninstall_") {
				continue
			}
			var args []struct {
				Steps []json.RawMessage `json:"steps"`
			}
			if err := json.Unmarshal(v, &args); err != nil || len(args) != 1 || len(args[0].Steps) != 1 {
				t.Errorf("%s = %s, want the args as brew lists them: [{steps: [...]}]", k, v)
			}
		}
	}
	if strings.Join(kinds, ",") != "app,uninstall_preflight_steps,uninstall_postflight_steps" {
		t.Errorf("receipt kinds = %v", kinds)
	}
	if err := l.ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(log)
	if err != nil || string(b) != "preflight-app-present\npostflight-app-gone\n" {
		t.Errorf("log = %q, %v, want the preflight step before the app moves back and the postflight step after", b, err)
	}
}

func TestUninstallStepsMayNeedSudoOnlyWhenUninstalling(t *testing.T) {
	l := newSudoLink(t)
	runs := l.recordRuns()
	t.Setenv("HOMEBREW_NO_SUDO", "")
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	artifacts := `[{"app":["App.app"]},{"uninstall_postflight_steps":[{"steps":[{"type":"run","sudo":true,"command":{"path":"/usr/sbin/tool"}}]}]}]`
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	if err := l.ti.in.Install(context.Background(), l.ti.cask(t, artifacts)); err != nil {
		t.Fatalf("brew checks sudo for install steps only: %v", err)
	}
	t.Setenv("HOMEBREW_NO_SUDO", "")
	if err := l.ti.in.Uninstall(context.Background(), "app", "1.0", false); err != nil {
		t.Fatal(err)
	}
	if c := sudoRun(t, *runs, "/usr/sbin/tool"); c.Privilege != sudo.Sudo {
		t.Errorf("command = %v, want it run with sudo", c)
	}
}

func TestInstallRefusesUninstallStepsBrwCannotRun(t *testing.T) {
	l := newSudoLink(t)
	err := l.ti.in.Install(context.Background(), l.ti.cask(t, `[{"app":["App.app"]},{"uninstall_preflight_steps":[{"steps":[{"type":"made_up_step"}]}]}]`))
	if err == nil || !strings.Contains(err.Error(), "made_up_step") {
		t.Errorf("err = %v", err)
	}
}
