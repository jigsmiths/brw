package poststep

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/sandbox"
	"github.com/jigsmiths/brw/internal/sudo"
)

type sandboxFixture struct {
	*sudoFixture
	warnings []string
}

func newSandboxFixture(t *testing.T) *sandboxFixture {
	t.Helper()
	f := &sandboxFixture{sudoFixture: newSudoFixture(t)}
	f.ctx.Staged = filepath.Join(f.prefix, "Caskroom/pkg/1.2.3")
	f.ctx.Token = "pkg"
	f.ctx.AppDir = t.TempDir()
	f.ctx.Sandbox = Sandboxed
	f.ctx.SandboxPolicy = NewSandboxPolicy(t.TempDir(), t.TempDir(), filepath.Join(f.prefix, "Homebrew"),
		func(m string) { f.warnings = append(f.warnings, m) })
	f.ctx.SandboxPolicy.Available = func() bool { return true }
	return f
}

func (f *sandboxFixture) profile(c sudo.Command) string {
	f.t.Helper()
	if c.Exe != sandbox.Exec || len(c.Args) < 3 || c.Args[0] != "-p" {
		f.t.Fatalf("command = %v, want sandbox-exec -p profile program...", c)
	}
	return c.Args[1]
}

func TestSandboxedRunUsesBrewsProfileAndDeniesNetworkByDefault(t *testing.T) {
	f := newSandboxFixture(t)
	work := filepath.Join(f.dir, "work")
	f.mustRun(fmt.Sprintf(`[{"type":"run","command":{"path":"/usr/bin/tool"},"args":["{{token}}"],"env":{"A":"b"},
		"writable_paths":[{"path":%q}],"stdout_path":{"path":%q}}]`, work, filepath.Join(f.dir, "out/stdout.txt")))
	c := f.only()
	p := f.profile(c)
	real := func(path string) string {
		rest := ""
		for p := path; ; p = filepath.Dir(p) {
			if r, err := filepath.EvalSymlinks(p); err == nil {
				return filepath.Join(r, rest)
			}
			rest = filepath.Join(filepath.Base(p), rest)
		}
	}
	for _, want := range []string{
		"(deny network*)",
		`(allow file-read* (subpath "` + real(f.ctx.Staged) + `"))`,
		`(allow file-write* (subpath "` + real(work) + `"))`,
		`(allow file-write* (subpath "` + real(filepath.Join(f.dir, "out")) + `"))`,
		`(allow file-write* (subpath "` + real(filepath.Join(f.prefix, "Caskroom/pkg")) + `"))`,
		`(allow file-write* (subpath "` + real(f.ctx.AppDir) + `"))`,
		`(allow file-write* (subpath "` + real(filepath.Join(f.prefix, "bin")) + `"))`,
		`(deny file-write* (literal "` + real(filepath.Join(f.prefix, "bin/brew")) + `"))`,
		`(deny file-write* (subpath "` + real(filepath.Join(f.prefix, "var/homebrew/locks")) + `"))`,
		`(deny file-read* (subpath "` + real(filepath.Join(f.prefix, "var/homebrew/locks")) + `"))`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("the profile lacks %q:\n%s", want, p)
		}
	}
	if rest := c.Args[2:]; rest[0] != "/usr/bin/tool" || !slices.Equal(rest[1:], []string{"pkg"}) {
		t.Errorf("sandboxed command = %q", rest)
	}
	var home string
	for _, e := range c.Env {
		if v, ok := strings.CutPrefix(e, "HOME="); ok {
			home = v
		}
	}
	if home == "" || !slices.Contains(c.Env, "A=b") {
		t.Errorf("env = %q, want HOME set to a scratch directory and the step's variables", c.Env)
	}
	if !strings.Contains(p, `(allow file-write* (subpath "`+real(home)+`"))`) {
		t.Errorf("the profile does not let the command write its home %s", home)
	}
}

func TestSandboxedRunAllowsTheNetworkOnlyWhenTheStepAsks(t *testing.T) {
	f := newSandboxFixture(t)
	f.mustRun(`[{"type":"run","command":{"path":"/usr/bin/tool"},"network_access":true}]`)
	p := f.profile(f.only())
	if strings.Contains(p, "(deny network*)") || !strings.Contains(p, "mDNSResponder") {
		t.Errorf("a step with network_access gets the network:\n%s", p)
	}
}

func TestSudoRunsAndOtherSystemsAreNotSandboxed(t *testing.T) {
	f := newSandboxFixture(t)
	f.mustRun(`[{"type":"run","sudo":true,"command":{"path":"/usr/bin/tool"}}]`)
	if c := f.only(); c.Exe != "/usr/bin/tool" || c.Privilege != sudo.Sudo {
		t.Errorf("a sudo step runs in the parent, as brew's broker does: %v", c)
	}
	g := newSandboxFixture(t)
	g.ctx.OS = "linux"
	g.mustRun(`[{"type":"run","command":{"path":"/usr/bin/true"}}]`)
	if len(g.stub.cmds) != 0 {
		t.Errorf("brew has no sandbox on linux, but the runner got %v", g.stub.cmds)
	}
}

func TestTheSandboxModeMustBeChosen(t *testing.T) {
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

func TestSandboxUnavailableWarnsAndRunsTheCommandPlainly(t *testing.T) {
	f := newSandboxFixture(t)
	f.ctx.SandboxPolicy.Available = func() bool { return false }
	f.mustRun(`[{"type":"run","command":{"path":"/usr/bin/true"}}]`)
	if len(f.stub.cmds) != 0 {
		t.Errorf("the command went through the sandbox runner: %v", f.stub.cmds)
	}
	if want := "Sandbox unavailable: running cask artifact operations without sandboxing!"; !slices.Equal(f.warnings, []string{want}) {
		t.Errorf("warnings = %q, want %q", f.warnings, want)
	}
}
