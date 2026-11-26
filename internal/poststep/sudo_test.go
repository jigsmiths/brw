package poststep

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/jigsmiths/brw/internal/sudo"
)

type stubSudo struct {
	cmds    []sudo.Command
	removed []string
	err     error
}

func (s *stubSudo) run(_ context.Context, c sudo.Command) (sudo.Result, error) {
	s.cmds = append(s.cmds, c)
	return sudo.Result{}, s.err
}

func (s *stubSudo) remove(_ context.Context, path string) error {
	s.removed = append(s.removed, path)
	return nil
}

type sudoFixture struct {
	*fixture
	stub   *stubSudo
	dir    string
	source string
}

func newSudoFixture(t *testing.T) *sudoFixture {
	t.Helper()
	f := &sudoFixture{fixture: newFixture(t), stub: &stubSudo{}, dir: t.TempDir(), source: filepath.Join(t.TempDir(), "kubectl")}
	f.ctx.Sudo, f.ctx.Remove = f.stub.run, f.stub.remove
	if err := os.WriteFile(f.source, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *sudoFixture) target() string { return filepath.Join(f.dir, "kubectl") }

func (f *sudoFixture) step(extra string) string {
	return fmt.Sprintf(`[{"type":"symlink","source":{"path":%q},"target":{"path":%q}%s}]`, f.source, f.target(), extra)
}

func (f *sudoFixture) uninstall(steps string) error {
	f.t.Helper()
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(steps), &raw); err != nil {
		f.t.Fatal(err)
	}
	return Uninstall(context.Background(), f.ctx, raw)
}

func (f *sudoFixture) lock() {
	f.t.Helper()
	if os.Geteuid() == 0 {
		f.t.Skip("root can write to a read-only directory")
	}
	if err := os.Chmod(f.dir, 0o555); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = os.Chmod(f.dir, 0o755) })
}

func (f *sudoFixture) wantLn(args ...string) {
	f.t.Helper()
	if len(f.stub.cmds) != 1 {
		f.t.Fatalf("runner got %d commands, want 1: %v", len(f.stub.cmds), f.stub.cmds)
	}
	c := f.stub.cmds[0]
	if c.Exe != "/bin/ln" || c.Privilege != sudo.Sudo || !c.MustSucceed || !slices.Equal(c.Args, args) {
		f.t.Errorf("runner got %v, want sudo /bin/ln %q", c, args)
	}
}

func (f *sudoFixture) linked() bool {
	got, err := os.Readlink(f.target())
	return err == nil && got == f.source
}

func TestUnsupportedKeepsSudoOnlyForSymlinks(t *testing.T) {
	raw := func(s string) []json.RawMessage {
		var r []json.RawMessage
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, value := range []string{`true`, `"if_needed"`, `false`, `null`} {
		if got := Unsupported(raw(`[{"type":"symlink","sudo":` + value + `,"source":{"path":"/a"},"target":{"path":"/b"}}]`)); len(got) != 0 {
			t.Errorf("symlink with sudo %s: Unsupported = %v", value, got)
		}
	}
	got := Unsupported(raw(`[{"type":"remove","sudo":true,"paths":[]},{"type":"run","sudo":"if_needed","command":{"path":"x"}}]`))
	if want := []string{"remove with sudo", "run with sudo"}; !slices.Equal(got, want) {
		t.Errorf("Unsupported = %v, want %v", got, want)
	}
}

func TestSymlinkWithSudoTrueRunsLnThroughSudo(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(f.step(`,"sudo":true,"force":true`))
	f.wantLn("-s", "-f", f.source, f.target())
	if _, err := os.Lstat(f.target()); err == nil {
		t.Error("brw linked by itself instead of through sudo")
	}
}

func TestSymlinkWithSudoTrueOmitsForceWhenNotAsked(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(f.step(`,"sudo":true`))
	f.wantLn("-s", f.source, f.target())
}

func TestSymlinkIfNeededLinksDirectlyInAWritableDirectory(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(f.step(`,"sudo":"if_needed","force":true`))
	if len(f.stub.cmds) != 0 || !f.linked() {
		t.Errorf("commands = %v, linked = %t; want a direct link", f.stub.cmds, f.linked())
	}
	other := filepath.Join(f.dir, "other")
	if err := os.WriteFile(other, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.target()); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(other, f.target()); err != nil {
		t.Fatal(err)
	}
	f.mustRun(f.step(`,"sudo":"if_needed","force":true`))
	if !f.linked() {
		t.Error("force did not replace the old link")
	}
	if err := f.run(f.step(`,"sudo":"if_needed"`)); err == nil {
		t.Error("linking over an existing path without force should fail")
	}
}

func TestSymlinkIfNeededUsesSudoInAnUnwritableDirectory(t *testing.T) {
	f := newSudoFixture(t)
	f.lock()
	f.mustRun(f.step(`,"sudo":"if_needed","force":true`))
	f.wantLn("-s", "-f", f.source, f.target())
}

func TestSymlinkWithSudoFalseStaysInThePrefix(t *testing.T) {
	f := newSudoFixture(t)
	if err := f.run(f.step(`,"sudo":false`)); err == nil {
		t.Error("a plain symlink outside the prefix should still be refused")
	}
	if len(f.stub.cmds) != 0 {
		t.Errorf("runner got %v", f.stub.cmds)
	}
}

func TestSymlinkSudoFailureIsReported(t *testing.T) {
	f := newSudoFixture(t)
	f.stub.err = errors.New("sudo is disabled by HOMEBREW_NO_SUDO.")
	if err := f.run(f.step(`,"sudo":true`)); err == nil || !errors.Is(err, f.stub.err) {
		t.Errorf("err = %v", err)
	}
}

func TestSymlinkGuardsSkipTheStep(t *testing.T) {
	f := newSudoFixture(t)
	if err := os.Symlink(f.source, f.target()); err != nil {
		t.Fatal(err)
	}
	guard := fmt.Sprintf(`,"sudo":"if_needed","force":true,"guards":[{"path":%q,"condition":"unless_exists","id":"1"}]`, f.target())
	f.mustRun(f.step(guard))
	if len(f.stub.cmds) != 0 {
		t.Errorf("a step whose guard fails ran: %v", f.stub.cmds)
	}
	if err := os.Remove(f.target()); err != nil {
		t.Fatal(err)
	}
	f.mustRun(f.step(guard))
	if !f.linked() {
		t.Error("the step should run once the guard passes")
	}
}

func TestSymlinkGlobWithSudoLinksEachSource(t *testing.T) {
	f := newSudoFixture(t)
	src := t.TempDir()
	for _, n := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(src, n), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.mustRun(fmt.Sprintf(`[{"type":"symlink","sudo":true,"source_glob":true,"source":{"path":%q},"target":{"path":%q}}]`,
		filepath.Join(src, "*"), filepath.Join(f.dir, "links")))
	var got [][]string
	for _, c := range f.stub.cmds {
		got = append(got, c.Args)
	}
	want := [][]string{
		{"-s", filepath.Join(src, "a"), filepath.Join(f.dir, "links", "a")},
		{"-s", filepath.Join(src, "b"), filepath.Join(f.dir, "links", "b")},
	}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("ln calls = %q, want %q", got, want)
	}
}

func TestUninstallRemovesALinkDirectlyWhenTheDirectoryIsWritable(t *testing.T) {
	f := newSudoFixture(t)
	if err := os.Symlink(f.source, f.target()); err != nil {
		t.Fatal(err)
	}
	if err := f.uninstall(f.step(`,"sudo":"if_needed","uninstall":true`)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(f.target()); err == nil || len(f.stub.removed) != 0 || len(f.stub.cmds) != 0 {
		t.Errorf("link left or privileged remove used: removed %v, commands %v", f.stub.removed, f.stub.cmds)
	}
}

func TestUninstallRemovesALinkWithPermissionsWhenSudoIsNeeded(t *testing.T) {
	for name, extra := range map[string]string{
		"true":      `,"sudo":true,"uninstall":true`,
		"if_needed": `,"sudo":"if_needed","uninstall":true`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newSudoFixture(t)
			if err := os.Symlink(f.source, f.target()); err != nil {
				t.Fatal(err)
			}
			if name == "if_needed" {
				f.lock()
			}
			if err := f.uninstall(f.step(extra)); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.stub.removed, []string{f.target()}) {
				t.Errorf("removed = %v, want the link", f.stub.removed)
			}
		})
	}
}

func TestUninstallLeavesALinkThatMovedOrIsNotMarked(t *testing.T) {
	f := newSudoFixture(t)
	if err := os.Symlink("/elsewhere", f.target()); err != nil {
		t.Fatal(err)
	}
	for _, extra := range []string{`,"sudo":true,"uninstall":true`, `,"sudo":true`} {
		if err := f.uninstall(f.step(extra)); err != nil {
			t.Fatal(err)
		}
	}
	if len(f.stub.removed) != 0 {
		t.Errorf("removed = %v", f.stub.removed)
	}
}
