package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/selfupdate"
)

func TestBuildVersion(t *testing.T) {
	for _, tt := range []struct {
		name                      string
		stamped, module, revision string
		want                      string
	}{
		{"stamped at release", "1.2.3", "(devel)", "", "1.2.3"},
		{"stamped wins over the module", "1.2.3", "v9.9.9", "", "1.2.3"},
		{"go install", "", "v1.2.3", "abcdef1234", "v1.2.3"},
		{"local build", "", "(devel)", "abcdef1234", ""},
		{"nothing known", "", "", "", ""},
	} {
		if got := buildVersion(tt.stamped, tt.module); got != tt.want {
			t.Errorf("%s: buildVersion(%q, %q) = %q, want %q", tt.name, tt.stamped, tt.module, got, tt.want)
		}
	}
}

func TestVersionLine(t *testing.T) {
	for _, tt := range []struct{ version, revision, want string }{
		{"1.2.3", "abcdef1234", "brw 1.2.3\n"},
		{"", "abcdef1234", "brw dev (abcdef1)\n"},
		{"", "abcdef1", "brw dev (abcdef1)\n"},
		{"", "abc", "brw dev\n"},
		{"", "", "brw dev\n"},
	} {
		if got := versionLine(tt.version, tt.revision); got != tt.want {
			t.Errorf("versionLine(%q, %q) = %q, want %q", tt.version, tt.revision, got, tt.want)
		}
	}
}

func TestVersionCommand(t *testing.T) {
	out, err := runCmd(t, &env{host: linux}, "version")
	if err != nil || !strings.HasPrefix(out, "brw ") || strings.Count(out, "\n") != 1 {
		t.Errorf("version = %q, %v", out, err)
	}
	if _, err := runCmd(t, &env{host: linux}, "version", "x"); err == nil {
		t.Error("version took an argument")
	}
}

func TestSBOMCreatorUsesTheStampedVersion(t *testing.T) {
	old := stampedVersion
	stampedVersion = "2.0.0"
	t.Cleanup(func() { stampedVersion = old })
	if got := sbomCreator(); got != "Tool: https://github.com/jigsmiths/brw@2.0.0" {
		t.Errorf("sbomCreator() = %q", got)
	}
}

func TestTheRealEnvUpdatesBrw(t *testing.T) {
	if newEnv(linux).selfUpdate == nil {
		t.Error("brw would not update itself")
	}
}

func TestSelfUpdateOfALocalBuild(t *testing.T) {
	old := stampedVersion
	stampedVersion = ""
	t.Cleanup(func() { stampedVersion = old })
	if _, err := selfUpdateBrw(context.Background()); !errors.Is(err, selfupdate.ErrNotRelease) {
		t.Errorf("error = %v, want ErrNotRelease", err)
	}
}
