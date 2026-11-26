package poststep

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/sandbox"
	"github.com/jigsmiths/brw/internal/sudo"
)

const childHelperEnv = "BRW_TEST_SANDBOX_CHILD"

func TestChildHelper(t *testing.T) {
	if os.Getenv(childHelperEnv) == "" {
		t.Skip("runs only as the sandboxed child of another test")
	}
	args := os.Args
	os.Exit(ChildMain(args[slices.Index(args, "--")+1:], os.Stdout, os.Stderr))
}

type sandboxFixture struct {
	*sudoFixture
	warnings []string
}

func newSandboxFixture(t *testing.T) *sandboxFixture {
	t.Helper()
	f := &sandboxFixture{sudoFixture: newSudoFixture(t)}
	short, err := os.MkdirTemp("", "sb")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(short) })
	f.ctx.Staged = filepath.Join(f.prefix, "Caskroom/pkg/1.2.3")
	f.ctx.Token = "pkg"
	f.ctx.AppDir = t.TempDir()
	f.ctx.Temp = short
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f.ctx.Sandbox = Sandboxed
	f.ctx.SandboxPolicy = NewSandboxPolicy(f.prefix, t.TempDir(), t.TempDir(), filepath.Join(f.prefix, "Homebrew"),
		[]string{exe, "-test.run=^TestChildHelper$", "--"}, nil, false, func(m string) { f.warnings = append(f.warnings, m) })
	f.ctx.SandboxPolicy.Inherited = func() (bool, error) { return false, nil }
	return f
}

func (f *sandboxFixture) skipUnlessRealSandbox(t *testing.T) {
	t.Helper()
	if f.ctx.OS != "darwin" || !sandbox.Available() {
		t.Skip("sandbox-exec is the point of this test and only macOS has it")
	}
	if sandbox.Nested() {
		t.Skip("this process is already sandboxed, and a sandbox cannot be nested")
	}
	t.Setenv(childHelperEnv, "1")
}

func TestAFileStepIsConfinedByTheSandbox(t *testing.T) {
	f := newSandboxFixture(t)
	f.skipUnlessRealSandbox(t)
	allowed := filepath.Join(f.prefix, "bin/made")
	denied := filepath.Join(f.prefix, "var/homebrew/locks/forbidden")
	if err := os.MkdirAll(filepath.Dir(denied), 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"touch","path":{"path":%q}}]`, allowed))
	if _, err := os.Stat(allowed); err != nil {
		t.Fatalf("a step inside its allowed paths failed: %v", err)
	}
	err := f.run(fmt.Sprintf(`[{"type":"touch","path":{"path":%q}}]`, denied))
	if err == nil || !strings.Contains(err.Error(), "An exception occurred within a child process:") {
		t.Errorf("err = %v, want the child's report", err)
	}
	if _, statErr := os.Stat(denied); statErr == nil {
		t.Error("the sandboxed file step wrote a path the profile denies")
	}
}

func TestAPrivilegedStepRunsInTheParentInOrder(t *testing.T) {
	f := newSandboxFixture(t)
	f.skipUnlessRealSandbox(t)
	first, last := filepath.Join(f.prefix, "bin/first"), filepath.Join(f.prefix, "bin/last")
	f.mustRun(fmt.Sprintf(`[{"type":"touch","path":{"path":%q}},
		{"type":"run","sudo":true,"command":{"path":"/usr/sbin/tool"},"args":["{{token}}"]},
		{"type":"touch","path":{"path":%q}}]`, first, last))
	c := f.only()
	if c.Exe != "/usr/sbin/tool" || c.Privilege != sudo.Sudo || !slices.Equal(c.Args, []string{"pkg"}) {
		t.Errorf("the parent ran %v", c)
	}
	for _, p := range []string{first, last} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("the child did not make %s: %v", p, err)
		}
	}
}

type pipe struct {
	child  *childChannel
	parent net.Conn
}

func newPipe() pipe {
	a, b := net.Pipe()
	return pipe{child: &childChannel{conn: a, in: bufio.NewReader(a)}, parent: b}
}

func (f *sandboxFixture) serveRequests(steps string, send func(c *childChannel) error) (res *childResult, childErr error) {
	raw := rawSteps(f.t, steps)
	p := newPipe()
	res = &childResult{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.runner().serve(p.parent, raw, phaseInstall, res)
		_ = p.parent.Close()
	}()
	childErr = send(p.child)
	_ = p.child.conn.Close()
	<-done
	return res, childErr
}

func (f *sandboxFixture) runner() *runner {
	return &runner{ctx: context.Background(), c: f.ctx, guards: map[string]bool{}}
}

func TestParentAnswersPrivilegedRequestsInOrder(t *testing.T) {
	f := newSandboxFixture(t)
	steps := `[{"type":"run","sudo":true,"command":{"path":"/a"}},{"type":"touch","path":{"path":"/x"}},{"type":"run","sudo":true,"command":{"path":"/b"}}]`
	res, err := f.serveRequests(steps, func(c *childChannel) error {
		if err := c.request(0); err != nil {
			return err
		}
		return c.request(2)
	})
	if err != nil || res.handlerErr != nil {
		t.Fatalf("child err = %v, parent err = %v", err, res.handlerErr)
	}
	if len(f.stub.cmds) != 2 || f.stub.cmds[0].Exe != "/a" || f.stub.cmds[1].Exe != "/b" {
		t.Errorf("parent ran %v", f.stub.cmds)
	}
}

func TestParentRefusesWhatBrewRefuses(t *testing.T) {
	steps := `[{"type":"run","sudo":true,"command":{"path":"/a"}},{"type":"touch","path":{"path":"/x"}}]`
	for name, tt := range map[string]struct {
		send func(c *childChannel) error
		want string
	}{
		"out of order":   {func(c *childChannel) error { _ = c.request(0); return c.request(0) }, "Privileged cask install steps must be requested in order."},
		"not privileged": {func(c *childChannel) error { return c.request(1) }, "Cask install step 1 is not privileged."},
		"no such step":   {func(c *childChannel) error { return c.request(7) }, "Invalid privileged cask install step index: 7"},
		"not a request": {func(c *childChannel) error {
			_, err := fmt.Fprintln(c.conn, `{"type":"other"}`)
			_ = c.request(0)
			return err
		}, "Invalid privileged cask child message."},
		"not an integer": {func(c *childChannel) error {
			_, err := fmt.Fprintln(c.conn, `{"type":"`+privilegedRequest+`","index":"0"}`)
			_ = c.request(0)
			return err
		}, `Invalid privileged cask install step index: "0"`},
	} {
		t.Run(name, func(t *testing.T) {
			f := newSandboxFixture(t)
			res, childErr := f.serveRequests(steps, tt.send)
			if res.handlerErr == nil || res.handlerErr.Error() != tt.want {
				t.Errorf("parent err = %v, want %q", res.handlerErr, tt.want)
			}
			if name != "not a request" && name != "not an integer" && (childErr == nil || !strings.Contains(childErr.Error(), "The parent process failed to run privileged cask install step")) {
				t.Errorf("child err = %v, want the failure report", childErr)
			}
		})
	}
}

func TestChildErrorsAreCollectedForTheParent(t *testing.T) {
	f := newSandboxFixture(t)
	res, _ := f.serveRequests(`[]`, func(c *childChannel) error {
		report, _ := json.Marshal(map[string]string{"json_class": "RuntimeError", "m": "boom"})
		_, err := fmt.Fprintf(c.conn, "%s\n", report)
		return err
	})
	if err := childError(res.errData); err == nil || err.Error() != "An exception occurred within a child process:\nRuntimeError: boom" {
		t.Errorf("err = %v", err)
	}
}

func TestRequestOrderHasNoRequestYet(t *testing.T) {
	var order requestOrder
	if !order.permits(0) {
		t.Error("the first request, even for step 0, must be allowed")
	}
	order = order.after(0)
	if order.permits(0) || !order.permits(1) {
		t.Error("after step 0 only a later step may be requested")
	}
}

func TestAChildWithoutAChannelCannotRunPrivilegedSteps(t *testing.T) {
	if err := (noChannel{}).request(3); err == nil || err.Error() != "Privileged cask install step 3 requires a parent message channel." {
		t.Errorf("err = %v", err)
	}
}

func TestChildRefusesABadChecksumAndAMissingPipe(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "payload.json")
	if err := os.WriteFile(body, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := runChild([]string{body, "deadbeef"}, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v", err)
	}
	t.Setenv(errorPipeEnv, "")
	if _, err := dialParent(); err == nil {
		t.Error("the child should refuse to run without its parent's socket")
	}
}

func TestUnsandboxedRunsInProcessAndTheModeMustBeChosen(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.Sandbox = 0
	if err := f.run(`[{"type":"run","command":{"path":"/usr/bin/true"}}]`); err == nil {
		t.Error("a context without a sandbox mode should be refused")
	}
	f.ctx.Sandbox = Unsandboxed
	f.mustRun(`[{"type":"run","command":{"path":"/usr/bin/true"}}]`)
	if len(f.stub.cmds) != 0 {
		t.Errorf("an unsandboxed step reached the runner: %v", f.stub.cmds)
	}
}

func TestSandboxUnavailableWarnsAndRunsInProcess(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.OS = "darwin"
	f.ctx.SandboxPolicy.Available = func() bool { return false }
	f.mustRun(`[{"type":"run","command":{"path":"/usr/bin/true"}}]`)
	if want := "Sandbox unavailable: running cask artifact operations without sandboxing!"; !slices.Equal(f.warnings, []string{want}) {
		t.Errorf("warnings = %q, want %q", f.warnings, want)
	}
}

func TestSandboxIsSkippedInsideAnotherBrwSandboxAndOnLinux(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.SandboxPolicy.Inherited = func() (bool, error) { return true, nil }
	f.mustRun(`[{"type":"run","command":{"path":"/usr/bin/true"}}]`)
	g := newSandboxFixture(t)
	g.ctx.OS = "linux"
	g.mustRun(`[{"type":"run","command":{"path":"/usr/bin/true"}}]`)
	if len(f.warnings)+len(g.warnings) != 0 {
		t.Errorf("warnings = %q %q", f.warnings, g.warnings)
	}
}

func TestProfileHasBrewsRulesForACask(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.Home = t.TempDir()
	raw := rawSteps(t, fmt.Sprintf(`[{"type":"run","network_access":true,"command":{"path":"/a"},"writable_paths":[{"path":%q}],"stdout_path":{"path":%q}}]`,
		filepath.Join(f.dir, "w"), filepath.Join(f.dir, "out/o")))
	tmp := t.TempDir()
	sb, err := f.runner().profile(raw, phaseInstall, filepath.Join(f.prefix, "home"), filepath.Join(f.prefix, "payload.json"), tmp)
	if err != nil {
		t.Fatal(err)
	}
	p := sb.Profile()
	real := func(path string) string {
		rest := ""
		for q := path; ; q = filepath.Dir(q) {
			if r, err := filepath.EvalSymlinks(q); err == nil {
				return filepath.Join(r, rest)
			}
			rest = filepath.Join(filepath.Base(q), rest)
		}
	}
	for _, want := range []string{
		`(allow file-read* (subpath "` + real(f.ctx.Staged) + `"))`,
		`(allow file-write* (subpath "` + real(filepath.Join(f.dir, "w")) + `"))`,
		`(allow file-write* (subpath "` + real(filepath.Join(f.dir, "out")) + `"))`,
		`(allow file-write* (subpath "` + real(filepath.Join(f.prefix, "Caskroom/pkg")) + `"))`,
		`(allow file-write* (subpath "` + real(f.ctx.AppDir) + `"))`,
		`(deny file-write* (literal "` + real(filepath.Join(f.prefix, "bin/brew")) + `"))`,
		`(deny file-write* (subpath "` + real(filepath.Join(f.prefix, "var/homebrew/tmp/.cellar")) + `"))`,
		`(deny file-read* (subpath "` + real(filepath.Join(f.prefix, "var/homebrew/locks")) + `"))`,
		`(allow file-read* (literal "` + real(filepath.Join(f.prefix, "payload.json")) + `"))`,
		`(allow network* network-outbound (subpath "` + real(tmp) + `"))`,
		"mDNSResponder",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the profile lacks %q", want)
		}
	}
	if strings.Contains(p, "(deny network*)") {
		t.Error("network_access was not honoured")
	}
}

func TestFormulaProfileGetsPostInstallRules(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.Token, f.ctx.Home = "", t.TempDir()
	if err := os.MkdirAll(filepath.Join(f.prefix, "libexec"), 0o755); err != nil {
		t.Fatal(err)
	}
	sb, err := f.runner().profile(nil, phaseInstall, "", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := sb.Profile()
	for _, want := range []string{"/Cellar/pkg\"))", "/etc\"))", "/libexec\"))", "xcrun_db", "(deny network*)"} {
		if !strings.Contains(p, want) {
			t.Errorf("the profile lacks %q:\n%s", want, p)
		}
	}
}
