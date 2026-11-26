package poststep

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func (f *sudoFixture) removeStep(extra string, paths ...string) string {
	specs := ""
	for i, p := range paths {
		if i > 0 {
			specs += ","
		}
		specs += fmt.Sprintf(`{"path":%q}`, p)
	}
	return fmt.Sprintf(`[{"type":"remove","paths":[%s]%s}]`, specs, extra)
}

func (f *sudoFixture) touch(name string) string {
	f.t.Helper()
	p := filepath.Join(f.dir, name)
	if err := os.WriteFile(p, []byte("data"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func TestRemoveWithSudoTrueGainsPermissionsForEveryMatch(t *testing.T) {
	f := newSudoFixture(t)
	a, b := f.touch("a.txt"), f.touch("b.txt")
	f.touch("c.log")
	f.mustRun(f.removeStep(`,"sudo":true,"recursive":true`, filepath.Join(f.dir, "*.txt")))
	if !slices.Equal(f.stub.removed, []string{a, b}) {
		t.Errorf("removed = %v, want %v", f.stub.removed, []string{a, b})
	}
	if len(f.stub.cmds) != 0 {
		t.Errorf("the removal runs through the privileged remove, got commands %v", f.stub.cmds)
	}
}

func TestRemoveWithSudoTrueNeedsAPrivilegedRemove(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.Remove = nil
	p := f.touch("a.txt")
	if err := f.run(f.removeStep(`,"sudo":true`, p)); err == nil {
		t.Error("remove with sudo and no privileged remove should fail")
	}
	if _, err := os.Lstat(p); err != nil {
		t.Error("the path was removed without sudo")
	}
}

func TestRemoveIfNeededRemovesDirectlyInAWritableDirectory(t *testing.T) {
	f := newSudoFixture(t)
	file := f.touch("a.txt")
	tree := filepath.Join(f.dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "deep"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.mustRun(f.removeStep(`,"sudo":"if_needed"`, file))
	f.mustRun(f.removeStep(`,"sudo":"if_needed"`, tree))
	if _, err := os.Lstat(file); err == nil {
		t.Error("the file was not removed")
	}
	if _, err := os.Lstat(tree); err != nil {
		t.Error("a directory must survive a non-recursive remove, like FileUtils.rm_f")
	}
	f.mustRun(f.removeStep(`,"sudo":"if_needed","recursive":true`, tree))
	if _, err := os.Lstat(tree); err == nil {
		t.Error("recursive remove left the tree")
	}
	if len(f.stub.removed) != 0 || len(f.stub.cmds) != 0 {
		t.Errorf("privileged remove used for a writable directory: %v %v", f.stub.removed, f.stub.cmds)
	}
}

func TestRemoveIfNeededUsesSudoInAnUnwritableDirectory(t *testing.T) {
	f := newSudoFixture(t)
	p := f.touch("a.txt")
	f.lock()
	f.mustRun(f.removeStep(`,"sudo":"if_needed"`, p))
	if !slices.Equal(f.stub.removed, []string{p}) {
		t.Errorf("removed = %v, want %v", f.stub.removed, []string{p})
	}
}

func TestRemoveWithSudoFalseStillStaysInThePrefix(t *testing.T) {
	f := newSudoFixture(t)
	p := f.touch("a.txt")
	if err := f.run(f.removeStep(`,"sudo":false`, p)); err == nil {
		t.Error("a plain remove outside the prefix should still be refused")
	}
	if len(f.stub.removed) != 0 {
		t.Errorf("removed = %v", f.stub.removed)
	}
}

func TestRemoveWithSudoFiltersOnSymlinkTargetAndContent(t *testing.T) {
	f := newSudoFixture(t)
	hit := filepath.Join(f.dir, "hit")
	miss := filepath.Join(f.dir, "miss")
	file := f.touch("file.txt")
	if err := os.Symlink("/Applications/Docker.app/x", hit); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/elsewhere", miss); err != nil {
		t.Fatal(err)
	}
	f.mustRun(f.removeStep(`,"sudo":true,"symlink_target_contains":"Docker.app"`, hit, miss, file))
	if !slices.Equal(f.stub.removed, []string{hit}) {
		t.Errorf("removed = %v, want only %v", f.stub.removed, []string{hit})
	}
	f.stub.removed = nil
	f.mustRun(f.removeStep(`,"sudo":true,"content_contains":"dat"`, hit, file))
	if !slices.Equal(f.stub.removed, []string{file}) {
		t.Errorf("removed = %v, want only %v", f.stub.removed, []string{file})
	}
}

func TestRemoveContentContainsFollowsSymlinksToFiles(t *testing.T) {
	f := newSudoFixture(t)
	file := f.touch("file.txt")
	link := filepath.Join(f.dir, "link")
	if err := os.Symlink(file, link); err != nil {
		t.Fatal(err)
	}
	f.mustRun(f.removeStep(`,"sudo":true,"content_contains":"data"`, link))
	if !slices.Equal(f.stub.removed, []string{link}) {
		t.Errorf("removed = %v, brew's path.file? follows the link", f.stub.removed)
	}
}

func TestRemoveWithSudoSkipsMissingPaths(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(f.removeStep(`,"sudo":true`, filepath.Join(f.dir, "gone")))
	if len(f.stub.removed) != 0 {
		t.Errorf("removed = %v", f.stub.removed)
	}
}
