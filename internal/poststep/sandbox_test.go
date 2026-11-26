package poststep

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	os.Exit(ChildMain(args[slices.Index(args, "--")+1:], os.Stdout, os.Stderr, Actions{echoAction: echoChild}))
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

const echoAction = "test_echo"

func echoChild(_ context.Context, c Context, extra json.RawMessage, _, _ io.Writer) error {
	var req struct{ Path, Body string }
	if err := json.Unmarshal(extra, &req); err != nil {
		return err
	}
	return os.WriteFile(req.Path, []byte(req.Body), 0o644)
}

func TestACustomActionRunsInTheSandboxedChild(t *testing.T) {
	f := newSandboxFixture(t)
	f.skipUnlessRealSandbox(t)
	allowed, denied := filepath.Join(f.prefix, "bin/echoed"), filepath.Join(f.prefix, "var/homebrew/locks/echoed")
	for _, dir := range []string{filepath.Dir(denied), filepath.Dir(allowed)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.ctx.Token = ""
	ran := false
	extra := func(path string) json.RawMessage {
		b, _ := json.Marshal(map[string]string{"Path": path, "Body": "x"})
		return b
	}
	if err := RunAction(t.Context(), f.ctx, echoAction, extra(allowed), func() error { ran = true; return nil }); err != nil || ran {
		t.Fatalf("err = %v, ran in process = %v", err, ran)
	}
	if _, err := os.Stat(allowed); err != nil {
		t.Errorf("the action could not write an allowed path: %v", err)
	}
	if err := RunAction(t.Context(), f.ctx, echoAction, extra(denied), func() error { return nil }); err == nil {
		t.Error("the action wrote a denied path")
	}
	if _, err := os.Stat(denied); err == nil {
		t.Error("the denied file exists")
	}
}

func TestRunActionRunsInProcessWhenUnsandboxed(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.Sandbox = Unsandboxed
	ran := false
	if err := RunAction(t.Context(), f.ctx, echoAction, nil, func() error { ran = true; return nil }); err != nil || !ran {
		t.Errorf("err = %v, ran = %v", err, ran)
	}
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
	if greeting, _ := p.child.in.ReadString('\n'); greeting != parentGreeting+"\n" {
		f.t.Errorf("the parent greeted with %q", greeting)
	}
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
	if err := runChild([]string{body, "deadbeef"}, nil, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Errorf("err = %v", err)
	}
	t.Setenv(errorPipeEnv, "")
	if _, _, err := dialParent(); err == nil {
		t.Error("the child should refuse to run without its parent's socket")
	}
}

func listenOn(t *testing.T, greet string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "hs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s")
	l, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = fmt.Fprint(conn, greet)
	}()
	t.Setenv(errorPipeEnv, socket)
}

func TestChildNeedsAParentThatAnswers(t *testing.T) {
	t.Run("the real greeting", func(t *testing.T) {
		listenOn(t, parentGreeting+"\n")
		conn, _, err := dialParent()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	})
	t.Run("a listener that does not greet", func(t *testing.T) {
		listenOn(t, "hello\n")
		if _, _, err := dialParent(); err == nil || err.Error() != "the parent process did not answer" {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("nobody listening", func(t *testing.T) {
		t.Setenv(errorPipeEnv, filepath.Join(t.TempDir(), "nothing"))
		if _, _, err := dialParent(); err == nil {
			t.Error("a socket nobody listens on must be refused")
		}
	})
}

func TestFormulaChildEnvironmentMirrorsBrew(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.Token = ""
	f.ctx.SandboxPolicy.Cache = "/cache"
	t.Setenv("HOMEBREW_PATH", "/opt/a:rel:/opt/b")
	t.Setenv("PATH", "/ignored")
	t.Setenv("CURL_HOME", "")
	t.Setenv("MY_SECRET_TOKEN", "x")
	env := f.runner().childEnv("/home", "/tmp/sb", "/tmp/sb/socket")
	has := func(kv string) bool { return slices.Contains(env, kv) }
	for _, want := range []string{
		"HOME=/home", "TMPDIR=/tmp/sb", "TEMP=/tmp/sb", "TMP=/tmp/sb", "HOMEBREW_TEMP=/tmp/sb",
		"_JAVA_OPTIONS=-Duser.home=/cache/java_cache -Djava.io.tmpdir=/tmp/sb",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GOENV=off", "PIP_CONFIG_FILE=/dev/null",
		"NPM_CONFIG_USERCONFIG=/dev/null", "CURL_HOME=", "PYTHONDONTWRITEBYTECODE=1", "XDG_CONFIG_HOME=/home/.config",
		"CARGO_REGISTRY_GLOBAL_MIN_PUBLISH_AGE=1 day", "BUNDLE_COOLDOWN=1", "BUNDLE_USER_CACHE=/cache/bundler_cache",
		"GOPATH=/cache/go_mod_cache", "ZIG_GLOBAL_CACHE_DIR=/cache/zig_cache", "HOMEBREW_ERROR_PIPE=/tmp/sb/socket",
	} {
		if !has(want) {
			t.Errorf("the formula child lacks %s", want)
		}
	}
	for _, e := range env {
		if strings.HasPrefix(e, "HOMEBREW_PATH=") || strings.HasPrefix(e, "MY_SECRET_TOKEN=") {
			t.Errorf("the child inherited %s", e)
		}
	}
	wd, _ := os.Getwd()
	if want := "PATH=/opt/a:" + filepath.Join(wd, "rel") + ":/opt/b"; !has(want) {
		t.Errorf("PATH = want %s in %q", want, env)
	}
}

func TestCaskChildEnvironmentHasOnlyWhatCaskArtifactRbSets(t *testing.T) {
	f := newSandboxFixture(t)
	env := f.runner().childEnv("/home", "/tmp/sb", "/tmp/sb/socket")
	for _, unwanted := range []string{"GOENV=off", "GIT_TERMINAL_PROMPT=0"} {
		if slices.Contains(env, unwanted) {
			t.Errorf("a cask child has %s", unwanted)
		}
	}
	for _, want := range []string{"HOME=/home", "TMPDIR=/tmp/sb", "HOMEBREW_CHILD_MESSAGE_CHANNEL=1"} {
		if !slices.Contains(env, want) {
			t.Errorf("a cask child lacks %s", want)
		}
	}
}

func TestPostinstallNetworkFollowsBrewsEnvironmentOverride(t *testing.T) {
	for value, want := range map[string]bool{"": true, "allow": true, "deny": false, "anything": false} {
		t.Setenv(postinstallNetwork, value)
		if got := postinstallNetworkAllowed(); got != want {
			t.Errorf("%s=%q: allowed = %v, want %v", postinstallNetwork, value, got, want)
		}
	}
}

func TestAFormulaGetsAHomeInTheSandboxDirectoryWithABazelrc(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.Token = ""
	tmp := t.TempDir()
	home, payloadPath, _, err := f.runner().scratch(payload{Action: formulaAction}, tmp)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(home) != tmp || !strings.HasPrefix(filepath.Base(home), "pkg-postinstall-") {
		t.Errorf("home = %s, want pkg-postinstall-* inside %s", home, tmp)
	}
	b, err := os.ReadFile(filepath.Join(home, ".bazelrc"))
	if err != nil || string(b) != "startup --output_user_root="+home+"/_bazel\n" {
		t.Errorf(".bazelrc = %q, %v", b, err)
	}
	if filepath.Dir(payloadPath) != tmp {
		t.Errorf("payload = %s", payloadPath)
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
	allowed, err := f.runner().profile(nil, phaseInstall, "", "", t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(allowed.Profile(), "(deny network*)") {
		t.Error("a formula's post_install may use the network unless told otherwise")
	}
	f.ctx.SandboxPolicy.NetworkAccess = false
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
