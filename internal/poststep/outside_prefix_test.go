package poststep

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type outsideFixture struct {
	*fixture
	dir string
}

func newOutsideFixture(t *testing.T) *outsideFixture {
	t.Helper()
	return &outsideFixture{fixture: newFixture(t), dir: t.TempDir()}
}

func (o *outsideFixture) path(name string) string { return filepath.Join(o.dir, name) }

func (o *outsideFixture) put(name, body string) string {
	o.t.Helper()
	p := o.path(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		o.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		o.t.Fatal(err)
	}
	return p
}

func (o *outsideFixture) read(p string) string {
	o.t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		o.t.Fatal(err)
	}
	return string(b)
}

func (o *outsideFixture) has(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func TestStepsReachPathsOutsideThePrefixAsBrewDoes(t *testing.T) {
	o := newOutsideFixture(t)
	spec := func(p string) string { return `{"path":"` + p + `"}` }

	t.Run("mkdir_p and mkdir", func(t *testing.T) {
		o.t = t
		deep := o.path("a/b/c")
		o.mustRun(`[{"type":"mkdir_p","path":` + spec(deep) + `},{"type":"mkdir","path":` + spec(o.path("a/d")) + `}]`)
		if !o.has(deep) || !o.has(o.path("a/d")) {
			t.Error("directories were not made")
		}
	})
	t.Run("touch", func(t *testing.T) {
		o.t = t
		p := o.path("t/file")
		o.mustRun(`[{"type":"touch","path":` + spec(p) + `}]`)
		o.mustRun(`[{"type":"touch","path":` + spec(p) + `}]`)
		if !o.has(p) {
			t.Error("file was not made")
		}
	})
	t.Run("write", func(t *testing.T) {
		o.t = t
		p := o.path("w/file")
		o.mustRun(`[{"type":"write","content":"hi","path":` + spec(p) + `}]`)
		if o.read(p) != "hi" {
			t.Error("content was not written")
		}
		o.mustRun(`[{"type":"write","content":"again","overwrite":true,"path":` + spec(p) + `}]`)
		if o.read(p) != "again" {
			t.Error("write with overwrite did not replace")
		}
	})
	t.Run("symlink", func(t *testing.T) {
		o.t = t
		src := o.put("s/src", "x")
		dst := o.path("s/link")
		o.mustRun(`[{"type":"symlink","source":` + spec(src) + `,"target":` + spec(dst) + `}]`)
		o.mustRun(`[{"type":"symlink","force":true,"source":` + spec(src) + `,"target":` + spec(dst) + `}]`)
		if got, _ := os.Readlink(dst); got != src {
			t.Errorf("link = %q", got)
		}
		glob := o.path("s/g")
		o.mustRun(`[{"type":"symlink","source_glob":true,"source":` + spec(o.path("s/sr?")) + `,"target":` + spec(glob) + `}]`)
		if got, _ := os.Readlink(glob); got != src {
			t.Errorf("glob link = %q", got)
		}
	})
	t.Run("remove", func(t *testing.T) {
		o.t = t
		file := o.put("r/file", "x")
		tree := o.path("r/tree")
		if err := os.MkdirAll(filepath.Join(tree, "d"), 0o755); err != nil {
			t.Fatal(err)
		}
		o.mustRun(`[{"type":"remove","paths":[` + spec(file) + `]}]`)
		o.mustRun(`[{"type":"remove","sudo":false,"paths":[` + spec(tree) + `]}]`)
		if o.has(file) {
			t.Error("file was not removed")
		}
		if !o.has(tree) {
			t.Error("a non-recursive remove must leave a directory, like rm_f")
		}
		o.mustRun(`[{"type":"remove","recursive":true,"paths":[` + spec(tree) + `]}]`)
		if o.has(tree) {
			t.Error("tree was not removed")
		}
	})
	t.Run("copy", func(t *testing.T) {
		o.t = t
		src := o.put("c/src", "data")
		o.mustRun(`[{"type":"copy","source":` + spec(src) + `,"target":` + spec(o.path("c/dst")) + `}]`)
		if o.read(o.path("c/dst")) != "data" {
			t.Error("file was not copied")
		}
		o.put("c/tree/deep/f", "deep")
		o.mustRun(`[{"type":"copy","recursive":true,"source":` + spec(o.path("c/tree")) + `,"target":` + spec(o.path("c/tree2")) + `}]`)
		if o.read(o.path("c/tree2/deep/f")) != "deep" {
			t.Error("tree was not copied")
		}
	})
	t.Run("copy from outside into the prefix and back", func(t *testing.T) {
		o.t = t
		src := o.put("cp/src", "data")
		o.mustRun(`[{"type":"copy","source":` + spec(src) + `,"target":{"base":"var","path":"in"}}]`)
		if o.fixture.read("var/in") != "data" {
			t.Error("file was not copied into the prefix")
		}
		o.mustRun(`[{"type":"copy","source":{"base":"var","path":"in"},"target":` + spec(o.path("cp/out")) + `}]`)
		if o.read(o.path("cp/out")) != "data" {
			t.Error("file was not copied out of the prefix")
		}
	})
	t.Run("move", func(t *testing.T) {
		o.t = t
		src := o.put("m/src", "data")
		o.mustRun(`[{"type":"move","source":` + spec(src) + `,"target":` + spec(o.path("m/dst")) + `}]`)
		if o.has(src) || o.read(o.path("m/dst")) != "data" {
			t.Error("file was not moved")
		}
		o.mustRun(`[{"type":"move","source":` + spec(o.path("m/dst")) + `,"target":{"base":"var","path":"moved"}}]`)
		if o.has(o.path("m/dst")) || o.fixture.read("var/moved") != "data" {
			t.Error("file was not moved into the prefix")
		}
		o.mustRun(`[{"type":"move","source":{"base":"var","path":"moved"},"target":` + spec(o.path("m/back")) + `}]`)
		if o.exists("var/moved") || o.read(o.path("m/back")) != "data" {
			t.Error("file was not moved out of the prefix")
		}
	})
	t.Run("link_dir and link_children", func(t *testing.T) {
		o.t = t
		o.put("l/src/a/f", "x")
		o.put("l/src/g", "y")
		o.mustRun(`[{"type":"link_dir","source":` + spec(o.path("l/src")) + `,"target":` + spec(o.path("l/dst")) + `}]`)
		if !o.has(o.path("l/dst/a/f")) || !o.has(o.path("l/dst/g")) {
			t.Error("link_dir did not link the tree")
		}
		o.mustRun(`[{"type":"link_children","prefix":"p-","source":` + spec(o.path("l/src")) + `,"target":` + spec(o.path("l/kids")) + `}]`)
		if !o.has(o.path("l/kids/p-g")) {
			t.Error("link_children did not link")
		}
	})
	t.Run("set_permissions", func(t *testing.T) {
		o.t = t
		p := o.put("perm/f", "x")
		o.mustRun(`[{"type":"set_permissions","permissions":"600","paths":[` + spec(p) + `]}]`)
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("mode = %v", fi.Mode())
		}
	})
	t.Run("run stdout_path", func(t *testing.T) {
		o.t = t
		out := o.path("run/out")
		o.mustRun(`[{"type":"run","command":{"path":"/bin/echo"},"args":["hello"],"stdout_path":` + spec(out) + `}]`)
		if strings.TrimSpace(o.read(out)) != "hello" {
			t.Error("stdout was not captured")
		}
	})
	t.Run("home and temp bases", func(t *testing.T) {
		o.t = t
		o.ctx.Home, o.ctx.Temp = o.path("home"), o.path("tmp")
		o.mustRun(`[{"type":"mkdir_p","path":{"base":"home","path":"x"}},{"type":"mkdir_p","path":{"base":"temp","path":"y"}}]`)
		if !o.has(o.path("home/x")) || !o.has(o.path("tmp/y")) {
			t.Error("home and temp bases did not resolve")
		}
	})
	t.Run("a parent climb leaves the prefix", func(t *testing.T) {
		o.t = t
		o.ctx.Home = o.dir
		o.mustRun(`[{"type":"touch","path":{"base":"home","path":"../` + filepath.Base(o.dir) + `/climb"}}]`)
		if !o.has(o.path("climb")) {
			t.Error("the climb did not land")
		}
	})
	t.Run("uninstall removes a symlink outside the prefix", func(t *testing.T) {
		o.t = t
		src := o.put("u/src", "x")
		dst := o.path("u/link")
		if err := os.Symlink(src, dst); err != nil {
			t.Fatal(err)
		}
		step := `[{"type":"symlink","uninstall":true,"source":` + spec(src) + `,"target":` + spec(dst) + `}]`
		if err := Uninstall(t.Context(), o.ctx, rawSteps(t, step)); err != nil {
			t.Fatal(err)
		}
		if o.has(dst) {
			t.Error("the link was not removed")
		}
	})
}

func rawSteps(t *testing.T, steps string) []json.RawMessage {
	t.Helper()
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(steps), &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}
