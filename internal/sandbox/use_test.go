package sandbox

import (
	"bytes"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
)

func TestDecideFollowsBrewsUseFor(t *testing.T) {
	const step = "running cask artifact operations"
	for name, tt := range map[string]struct {
		o       Options
		want    Decision
		message string
		wantErr string
	}{
		"inside a brw sandbox": {Options{Inherited: true, Available: true}, Skip, "", ""},
		"no sandbox-exec":      {Options{}, Skip, "Sandbox unavailable: running cask artifact operations without sandboxing!", ""},
		"ordinary":             {Options{Available: true}, Use, "", ""},
		"nested, not avoiding": {Options{Available: true, Nested: true}, Use, "", ""},
		"nested and avoiding":  {Options{Available: true, Nested: true, AvoidNested: true, DefaultPrefix: false}, Skip, "Running cask artifact operations without brw's sandbox; relying on the outer sandbox.", ""},
		"avoiding, not nested": {Options{Available: true, AvoidNested: true}, Use, "", ""},
		"avoiding in default":  {Options{Available: true, Nested: true, AvoidNested: true, DefaultPrefix: true, Prefix: "/opt/homebrew"}, Skip, "", "Refusing to skip the sandbox: `$HOMEBREW_AVOID_NESTED_SANDBOXING` is set inside another sandbox but brw is using its default prefix (/opt/homebrew); this is only supported in a custom prefix."},
		"avoiding, privileged": {Options{Available: true, Nested: true, AvoidNested: true, PrivilegedGroup: PrivilegedGroup{Name: "admin", Member: true}}, Skip, "", "Refusing to skip the sandbox: `$HOMEBREW_AVOID_NESTED_SANDBOXING` is set inside another sandbox but you are in the privileged `admin` group; this is only supported for an unprivileged user."},
	} {
		t.Run(name, func(t *testing.T) {
			d, msg, err := Decide(step, tt.o)
			if (err != nil) != (tt.wantErr != "") || (err != nil && err.Error() != tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if err != nil && d != Invalid {
				t.Errorf("an error came with decision %v, want Invalid", d)
			}
			if err == nil && (d != tt.want || msg != tt.message) {
				t.Errorf("decision = %v %q, want %v %q", d, msg, tt.want, tt.message)
			}
		})
	}
}

func TestNetworkRulesGetBrewsOutboundSuffix(t *testing.T) {
	var s Sandbox
	s.AllowNetworkSubpath(t.TempDir())
	if p := s.Profile(); !strings.Contains(p, `(allow network* network-outbound (subpath "`) {
		t.Errorf("profile lacks the allowed network path:\n%s", p)
	}
}

func TestInheritedIsFalseWithoutTheDescriptor(t *testing.T) {
	exe, _ := os.Executable()
	if got, err := Inherited(exe, exe); got || err != nil {
		t.Errorf("Inherited = %v, %v, want false", got, err)
	}
}

func TestArgvRunsTheSandboxThroughTheDisclaimerOnlyWhenAsked(t *testing.T) {
	s := Spec{Profile: "P", Exe: "/x", Args: []string{"a"}, DisclaimVia: []string{"/brw", DisclaimCommand}}
	if got := strings.Join(s.Argv(), " "); got != Exec+" -p P /x a" {
		t.Errorf("argv = %q", got)
	}
	s.Disclaim = true
	if got := strings.Join(s.Argv(), " "); got != "/brw "+DisclaimCommand+" "+Exec+" -p P /x a" {
		t.Errorf("argv = %q", got)
	}
}

func TestNoDecisionIsTheZeroValue(t *testing.T) {
	var zero Decision
	if zero != Invalid || zero == Use || zero == Skip {
		t.Errorf("the zero Decision = %v, want Invalid", zero)
	}
}

func TestLogDestinations(t *testing.T) {
	if NoLogFile.kept {
		t.Error("NoLogFile keeps a file")
	}
	if d := LogTo("/x"); !d.kept || d.path != "/x" {
		t.Errorf("LogTo = %+v", d)
	}
}

func TestRunLeavesTheExecutableOnTheInheritanceDescriptor(t *testing.T) {
	if runtime.GOOS != "darwin" || !Available() {
		t.Skip("sandbox-exec is the point of this test and only macOS has it")
	}
	if Nested() {
		t.Skip("this process is already sandboxed, and a sandbox cannot be nested")
	}
	var out bytes.Buffer
	code, err := Run(t.Context(), Spec{
		Profile: sandboxProbeProfile, Exe: "/bin/sh",
		Args:   []string{"-c", fmt.Sprintf("test -e /dev/fd/%d && echo held", InheritanceFD)},
		Stdout: &out,
	})
	if err != nil || code != 0 || strings.TrimSpace(out.String()) != "held" {
		t.Errorf("code = %d, err = %v, out = %q", code, err, out.String())
	}
}

func TestTheChildRunsOnItsOwnTerminal(t *testing.T) {
	if runtime.GOOS != "darwin" || !Available() {
		t.Skip("sandbox-exec is the point of this test and only macOS has it")
	}
	if Nested() {
		t.Skip("this process is already sandboxed, and a sandbox cannot be nested")
	}
	var out bytes.Buffer
	script := `echo out; echo err >&2; echo to-dev-tty > /dev/tty; test -t 0 && echo has-tty; tty; exit 7`
	code, err := Run(t.Context(), Spec{Profile: sandboxProbeProfile, Exe: "/bin/sh", Args: []string{"-c", script}, Stdout: &out})
	if err != nil || code != 7 {
		t.Fatalf("code = %d, err = %v", code, err)
	}
	got := strings.ReplaceAll(out.String(), "\r\n", "\n")
	for _, want := range []string{"out\n", "err\n", "to-dev-tty\n", "has-tty\n", "/dev/ttys"} {
		if !strings.Contains(got, want) {
			t.Errorf("the child's terminal output lacks %q: %q", want, got)
		}
	}
	if parent, err := os.Open("/dev/tty"); err == nil {
		defer func() { _ = parent.Close() }()
		if name := parent.Name(); strings.Contains(got, "\n"+name+"\n") {
			t.Errorf("the child ran on the parent's terminal %s", name)
		}
	}
}

func TestFilterLogDropsBytecodeWrites(t *testing.T) {
	in := "a deny file-write /x\nPython(12) deny file-write-create /x/y.pyc\nb deny network\n"
	if got := filterLog(in); got != "a deny file-write /x\nb deny network\n" {
		t.Errorf("filterLog = %q", got)
	}
}
