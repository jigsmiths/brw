package poststep

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/fsx"
)

type fixture struct {
	t      *testing.T
	prefix string
	ctx    Context
	log    bytes.Buffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	prefix := t.TempDir()
	root, err := fsx.Open(prefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	f := &fixture{t: t, prefix: prefix}
	f.ctx = Context{Root: root, Prefix: prefix, Name: "pkg", Version: "1.2.3", Keg: "Cellar/pkg/1.2.3-1", OS: "darwin", User: "tester", Log: &f.log}
	f.mkdir("Cellar/pkg/1.2.3-1")
	return f
}

func (f *fixture) mkdir(rel string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.prefix, rel), 0o755); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) write(rel, body string, mode os.FileMode) {
	f.t.Helper()
	f.mkdir(filepath.Dir(rel))
	if err := os.WriteFile(filepath.Join(f.prefix, rel), []byte(body), mode); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) read(rel string) string {
	f.t.Helper()
	b, err := os.ReadFile(filepath.Join(f.prefix, rel))
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func (f *fixture) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(f.prefix, rel))
	return err == nil
}

func (f *fixture) link(rel string) string {
	f.t.Helper()
	target, err := os.Readlink(filepath.Join(f.prefix, rel))
	if err != nil {
		f.t.Fatal(err)
	}
	return target
}

// run runs steps given as JSON, the way the API serves them.
func (f *fixture) run(steps string) error {
	f.t.Helper()
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(steps), &raw); err != nil {
		f.t.Fatal(err)
	}
	return Run(f.ctx, raw)
}

func (f *fixture) mustRun(steps string) {
	f.t.Helper()
	if err := f.run(steps); err != nil {
		f.t.Fatal(err)
	}
}

func TestMkdir(t *testing.T) {
	f := newFixture(t)
	f.mkdir("etc") // mkdir, unlike mkdir_p, needs its parent
	f.mustRun(`[
		{"type":"mkdir_p","path":{"base":"var","path":"log/pkg/deep"}},
		{"type":"mkdir_p","path":{"path":"{{HOMEBREW_PREFIX}}/lib/node_modules"}},
		{"type":"mkdir","path":{"base":"etc","path":"pkg"}}
	]`)
	for _, d := range []string{"var/log/pkg/deep", "lib/node_modules", "etc/pkg"} {
		if fi, err := os.Stat(filepath.Join(f.prefix, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s was not made: %v", d, err)
		}
	}
	if err := f.run(`[{"type":"mkdir","path":{"base":"etc","path":"pkg"}}]`); err == nil {
		t.Error("mkdir of an existing directory should fail, as brew's does")
	}
	f.mustRun(`[{"type":"mkdir_p","path":{"base":"etc","path":"pkg"}}]`)
}

func TestTouch(t *testing.T) {
	f := newFixture(t)
	f.mustRun(`[{"type":"touch","path":{"base":"var","path":"db/pkg/state"}}]`)
	if f.read("var/db/pkg/state") != "" {
		t.Error("touch made a file with content")
	}
	f.write("var/old", "keep", 0o644)
	past := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := os.Chtimes(filepath.Join(f.prefix, "var/old"), past, past); err != nil {
		t.Fatal(err)
	}
	f.mustRun(`[{"type":"touch","path":{"base":"var","path":"old"}}]`)
	fi, _ := os.Stat(filepath.Join(f.prefix, "var/old"))
	if f.read("var/old") != "keep" || time.Since(fi.ModTime()) > time.Minute {
		t.Errorf("touch should keep the content and update the time: %q %v", f.read("var/old"), fi.ModTime())
	}
}

func TestSymlink(t *testing.T) {
	f := newFixture(t)
	f.write("etc/ca-certificates/cert.pem", "certs", 0o644)
	steps := `[{"type":"symlink","force":true,
		"source":{"path":"{{etc}}/ca-certificates/cert.pem"},"target":{"path":"{{pkgetc}}/cert.pem"}}]`
	// etc and pkgetc here come from the formula, so {{etc}} is the prefix's etc
	f.ctx.Keg = "Cellar/pkg/1.2.3-1"
	f.mustRun(steps)
	if got := f.link("etc/pkg/cert.pem"); got != filepath.Join(f.prefix, "etc/ca-certificates/cert.pem") {
		t.Errorf("link = %q, want an absolute link to the certificates", got)
	}
	// force replaces what is there, a regular file or another link.
	f.mustRun(steps)
	f.write("etc/pkg/other", "x", 0o644)
	f.mustRun(`[{"type":"symlink","force":true,"source":{"base":"etc","path":"ca-certificates/cert.pem"},"target":{"base":"etc","path":"pkg/other"}}]`)
	if f.link("etc/pkg/other") != filepath.Join(f.prefix, "etc/ca-certificates/cert.pem") {
		t.Error("force did not replace the file")
	}
	// without force an existing target is an error.
	err := f.run(`[{"type":"symlink","source":{"base":"etc","path":"ca-certificates/cert.pem"},"target":{"base":"etc","path":"pkg/other"}}]`)
	if err == nil {
		t.Error("a symlink over an existing path without force should fail")
	}
	// a relative link keeps its text
	f.mustRun(`[{"type":"symlink","force":true,"source":{"base":"relative","path":"../ca-certificates"},"target":{"base":"etc","path":"pkg/rel"}}]`)
	if f.link("etc/pkg/rel") != "../ca-certificates" {
		t.Errorf("relative link = %q", f.link("etc/pkg/rel"))
	}
}

func TestSymlinkSourceGlob(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/pkg/1.2.3-1/libexec/bin/a", "a", 0o755)
	f.write("Cellar/pkg/1.2.3-1/libexec/bin/b", "b", 0o755)
	f.mustRun(`[{"type":"symlink","source_glob":true,"force":true,
		"source":{"base":"libexec","path":"bin/*"},"target":{"base":"homebrew_prefix","path":"bin"}}]`)
	for _, n := range []string{"a", "b"} {
		if got := f.link("bin/" + n); got != filepath.Join(f.prefix, "Cellar/pkg/1.2.3-1/libexec/bin/"+n) {
			t.Errorf("bin/%s -> %s", n, got)
		}
	}
	f.mustRun(`[{"type":"symlink","source_glob":true,"source":{"base":"libexec","path":"nothing/*"},"target":{"base":"homebrew_prefix","path":"bin"}}]`)
}

func TestRemove(t *testing.T) {
	f := newFixture(t)
	f.write("var/a.txt", "x", 0o644)
	f.write("var/dir/b.txt", "x", 0o644)
	f.write("var/keep.txt", "x", 0o644)
	f.mustRun(`[{"type":"remove","paths":[{"base":"var","path":"a.txt"},{"base":"var","path":"missing"}]}]`)
	if f.exists("var/a.txt") || !f.exists("var/keep.txt") {
		t.Error("remove removed the wrong files")
	}
	f.mustRun(`[{"type":"remove","recursive":true,"paths":[{"base":"var","path":"dir"}]}]`)
	if f.exists("var/dir") {
		t.Error("recursive remove left the directory")
	}
	// a plain remove leaves a directory alone
	f.mkdir("var/emptydir")
	f.mustRun(`[{"type":"remove","paths":[{"base":"var","path":"emptydir"}]}]`)
	if !f.exists("var/emptydir") {
		t.Error("a non-recursive remove took a directory")
	}
	if err := os.Symlink("/opt/elsewhere/bundler", filepath.Join(f.prefix, "var/gem-link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/opt/other/thing", filepath.Join(f.prefix, "var/other-link")); err != nil {
		t.Fatal(err)
	}
	f.mustRun(`[{"type":"remove","symlink_target_contains":"bundler","paths":[{"base":"var","path":"*-link"}]}]`)
	if f.exists("var/gem-link") || !f.exists("var/other-link") {
		t.Error("symlink_target_contains did not filter")
	}
	f.write("var/has", "needle inside", 0o644)
	f.write("var/hasnt", "nothing", 0o644)
	f.mustRun(`[{"type":"remove","content_contains":"needle","paths":[{"base":"var","path":"has*"}]}]`)
	if f.exists("var/has") || !f.exists("var/hasnt") {
		t.Error("content_contains did not filter")
	}
}

func TestCopy(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/pkg/1.2.3-1/share/pkg/conf", "from the keg", 0o644)
	f.mkdir("etc/pkg")
	f.mustRun(`[{"type":"copy","source":{"base":"pkgshare","path":"conf"},"target":{"base":"pkgetc","path":""}}]`)
	if f.read("etc/pkg/conf") != "from the keg" {
		t.Errorf("copy into a directory: %q", f.read("etc/pkg/conf"))
	}
	f.write("etc/pkg/conf", "edited", 0o644)
	if err := f.run(`[{"type":"copy","overwrite":false,"source":{"base":"pkgshare","path":"conf"},"target":{"base":"pkgetc","path":"conf"}}]`); err == nil {
		t.Error("overwrite false should refuse to replace")
	}
	if f.read("etc/pkg/conf") != "edited" {
		t.Error("a refused copy changed the file")
	}
	f.mustRun(`[{"type":"copy","source":{"base":"pkgshare","path":"conf"},"target":{"base":"pkgetc","path":"conf"}}]`)
	if f.read("etc/pkg/conf") != "from the keg" {
		t.Error("copy overwrites by default, as brew's does")
	}
	f.write("Cellar/pkg/1.2.3-1/share/pkg/tree/sub/x", "x", 0o644)
	f.mustRun(`[{"type":"copy","recursive":true,"source":{"base":"pkgshare","path":"tree"},"target":{"base":"var","path":"copied"}}]`)
	if f.read("var/copied/sub/x") != "x" {
		t.Error("recursive copy did not copy the tree")
	}
}

func TestMove(t *testing.T) {
	f := newFixture(t)
	f.write("var/src", "data", 0o644)
	f.mustRun(`[{"type":"move","source":{"base":"var","path":"src"},"target":{"base":"var","path":"dst"}}]`)
	if f.exists("var/src") || f.read("var/dst") != "data" {
		t.Error("move did not move")
	}
	f.write("var/src2", "new", 0o644)
	if err := f.run(`[{"type":"move","overwrite":false,"source":{"base":"var","path":"src2"},"target":{"base":"var","path":"dst"}}]`); err == nil {
		t.Error("move onto an existing path without overwrite should fail")
	}
	f.mustRun(`[{"type":"move","overwrite":true,"source":{"base":"var","path":"src2"},"target":{"base":"var","path":"dst"}}]`)
	if f.read("var/dst") != "new" {
		t.Error("overwrite did not replace")
	}
}

func TestLinkDirAndChildren(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/pkg/1.2.3-1/share/doc/a.txt", "a", 0o644)
	f.write("Cellar/pkg/1.2.3-1/share/doc/sub/b.txt", "b", 0o644)
	f.write("Cellar/pkg/1.2.3-1/share/doc/.DS_Store", "x", 0o644)
	f.mustRun(`[{"type":"link_dir","source":{"base":"share","path":"doc"},"target":{"base":"var","path":"docs"}}]`)
	if got := f.link("var/docs/a.txt"); got != "../../Cellar/pkg/1.2.3-1/share/doc/a.txt" {
		t.Errorf("a.txt -> %s, want a relative link like brew's install_symlink", got)
	}
	if fi, err := os.Lstat(filepath.Join(f.prefix, "var/docs/sub")); err != nil || !fi.IsDir() {
		t.Error("link_dir should make directories and link files")
	}
	if f.exists("var/docs/.DS_Store") {
		t.Error(".DS_Store was linked")
	}
	f.mustRun(`[{"type":"link_children","prefix":"x-","suffix":".lnk","source":{"base":"share","path":"doc"},"target":{"base":"var","path":"kids"}}]`)
	if got := dirNames(t, filepath.Join(f.prefix, "var/kids")); !reflect.DeepEqual(got, []string{"x-.DS_Store.lnk", "x-a.txt.lnk", "x-sub.lnk"}) {
		t.Errorf("children = %v: every child is linked, dotfiles too", got)
	}
	if got := f.link("var/kids/x-a.txt.lnk"); got != "../../Cellar/pkg/1.2.3-1/share/doc/a.txt" {
		t.Errorf("child link = %s", got)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func TestWrite(t *testing.T) {
	f := newFixture(t)
	f.mustRun(`[{"type":"write","content":"user={{user}} v={{version.major_minor}}\n","path":{"base":"etc","path":"pkg.conf"}}]`)
	if f.read("etc/pkg.conf") != "user=tester v=1.2\n" {
		t.Errorf("content = %q", f.read("etc/pkg.conf"))
	}
	f.write("etc/pkg.conf", "edited", 0o644)
	f.mustRun(`[{"type":"write","content":"fresh","path":{"base":"etc","path":"pkg.conf"}}]`)
	if f.read("etc/pkg.conf") != "edited" {
		t.Error("write replaced an existing file without overwrite")
	}
	f.mustRun(`[{"type":"write","overwrite":true,"content":"fresh","path":{"base":"etc","path":"pkg.conf"}}]`)
	if f.read("etc/pkg.conf") != "fresh" {
		t.Error("overwrite true did not replace")
	}
}

func TestWarn(t *testing.T) {
	f := newFixture(t)
	f.mustRun(`[{"type":"warn","message":"{{formula_name}} needs attention"}]`)
	if !strings.Contains(f.log.String(), "pkg needs attention") {
		t.Errorf("log = %q", f.log.String())
	}
}

func TestTemplateTokensAndBases(t *testing.T) {
	f := newFixture(t)
	f.mustRun(`[
		{"type":"touch","path":{"path":"{{HOMEBREW_PREFIX}}/t/a-{{formula_name}}-{{version}}-{{version.major}}-{{version.major_minor}}"}},
		{"type":"touch","path":{"path":"{{HOMEBREW_CELLAR}}/t-cellar"}},
		{"type":"touch","path":{"path":"{{prefix}}/t-prefix"}},
		{"type":"touch","path":{"path":"{{opt_prefix}}/t-opt"}},
		{"type":"touch","path":{"path":"{{bin}}/t-bin"}},
		{"type":"touch","path":{"path":"{{pkgshare}}/t-share"}},
		{"type":"touch","path":{"path":"{{var}}/t-var"}},
		{"type":"touch","path":{"path":"{{zsh_completion}}/t-zsh"}},
		{"type":"touch","path":{"base":"sbin","path":"t-sbin"}},
		{"type":"touch","path":{"base":"lib","path":"t-lib"}},
		{"type":"touch","path":{"base":"frameworks","path":"t-frameworks"}},
		{"type":"touch","path":{"base":"rack","path":"t-rack"}},
		{"type":"touch","path":{"base":"formula_pkgetc","formula":"other","path":"t-other"}},
		{"type":"touch","path":{"base":"formula_opt_prefix","formula":"homebrew/core/dep","path":"t-dep"}}
	]`)
	for _, p := range []string{
		"t/a-pkg-1.2.3-1-1.2", "Cellar/t-cellar", "Cellar/pkg/1.2.3-1/t-prefix", "opt/pkg/t-opt", "Cellar/pkg/1.2.3-1/bin/t-bin",
		"Cellar/pkg/1.2.3-1/share/pkg/t-share", "var/t-var", "Cellar/pkg/1.2.3-1/share/zsh/site-functions/t-zsh",
		"Cellar/pkg/1.2.3-1/sbin/t-sbin", "Cellar/pkg/1.2.3-1/lib/t-lib",
		"Cellar/pkg/1.2.3-1/Frameworks/t-frameworks", "Cellar/pkg/t-rack", "etc/other/t-other", "opt/dep/t-dep",
	} {
		if !f.exists(p) {
			t.Errorf("%s was not made", p)
		}
	}
}

func TestGuards(t *testing.T) {
	f := newFixture(t)
	f.write("var/present", "x", 0o644)
	f.mustRun(`[
		{"type":"touch","path":{"base":"var","path":"if-exists-yes"},"guards":[{"condition":"if_exists","base":"var","path":"present","id":"1"}]},
		{"type":"touch","path":{"base":"var","path":"if-exists-no"},"guards":[{"condition":"if_exists","base":"var","path":"absent","id":"2"}]},
		{"type":"touch","path":{"base":"var","path":"unless-yes"},"guards":[{"condition":"unless_exists","base":"var","path":"absent","id":"3"}]},
		{"type":"touch","path":{"base":"var","path":"unless-no"},"guards":[{"condition":"unless_exists","base":"var","path":"present","id":"4"}]},
		{"type":"touch","path":{"base":"var","path":"on-macos"},"guards":[{"condition":"on","value":"macos","id":"5"}]},
		{"type":"touch","path":{"base":"var","path":"on-linux"},"guards":[{"condition":"on","value":"linux","id":"6"}]},
		{"type":"touch","path":{"base":"var","path":"both"},"guards":[{"condition":"on","value":"macos","id":"7"},{"condition":"if_exists","base":"var","path":"present","id":"8"}]},
		{"type":"touch","path":{"base":"var","path":"one-fails"},"guards":[{"condition":"on","value":"macos","id":"9"},{"condition":"if_exists","base":"var","path":"absent","id":"10"}]}
	]`)
	want := map[string]bool{"if-exists-yes": true, "if-exists-no": false, "unless-yes": true, "unless-no": false, "on-macos": true, "on-linux": false, "both": true, "one-fails": false}
	for name, should := range want {
		if f.exists("var/"+name) != should {
			t.Errorf("%s exists = %v, want %v", name, !should, should)
		}
	}
}

func TestGuardResultsAreRememberedForTheRun(t *testing.T) {
	f := newFixture(t)
	// The first step creates the path that the guard of the second looked at, which
	// brew evaluated once up front.
	f.mustRun(`[
		{"type":"mkdir_p","path":{"base":"var","path":"db"},"guards":[{"condition":"unless_exists","base":"var","path":"db","id":"1"}]},
		{"type":"touch","path":{"base":"var","path":"db/marker"},"guards":[{"condition":"unless_exists","base":"var","path":"db","id":"1"}]}
	]`)
	if !f.exists("var/db/marker") {
		t.Error("a guard with the same id should keep the answer it first gave")
	}
}

func TestGuardsOnLinux(t *testing.T) {
	f := newFixture(t)
	f.ctx.OS = "linux"
	f.mustRun(`[
		{"type":"touch","path":{"base":"var","path":"mac"},"guards":[{"condition":"on","value":"macos","id":"1"}]},
		{"type":"touch","path":{"base":"var","path":"lin"},"guards":[{"condition":"on","value":"linux","id":"2"}]}
	]`)
	if f.exists("var/mac") || !f.exists("var/lin") {
		t.Error("guards did not follow the host")
	}
}

func TestNothingIsWrittenOutsideThePrefix(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(t.TempDir(), "outside")
	for name, step := range map[string]string{
		"mkdir_p":       `{"type":"mkdir_p","path":{"path":"` + outside + `"}}`,
		"touch":         `{"type":"touch","path":{"path":"` + outside + `/f"}}`,
		"symlink":       `{"type":"symlink","force":true,"source":{"base":"var","path":"x"},"target":{"path":"` + outside + `"}}`,
		"write":         `{"type":"write","content":"x","path":{"path":"` + outside + `"}}`,
		"remove":        `{"type":"remove","paths":[{"path":"` + outside + `"}]}`,
		"copy":          `{"type":"copy","source":{"base":"var","path":"x"},"target":{"path":"` + outside + `"}}`,
		"home base":     `{"type":"touch","path":{"base":"home","path":"x"}}`,
		"parent climbs": `{"type":"touch","path":{"base":"var","path":"../../escape"}}`,
		"temp base":     `{"type":"touch","path":{"base":"temp","path":"x"}}`,
		"relative base": `{"type":"touch","path":{"base":"relative","path":"x"}}`,
		"unknown base":  `{"type":"touch","path":{"base":"nonsense","path":"x"}}`,
	} {
		f.write("var/x", "x", 0o644)
		if err := f.run("[" + step + "]"); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
	if _, err := os.Lstat(outside); err == nil {
		t.Error("something was made outside the prefix")
	}
	if f.exists("../escape") {
		t.Error("a path with .. escaped")
	}
}

func TestUnsupported(t *testing.T) {
	raw := func(s string) []json.RawMessage {
		var r []json.RawMessage
		if err := json.Unmarshal([]byte(s), &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	got := Unsupported(raw(`[
		{"type":"mkdir_p","path":{"path":"x"}},
		{"type":"gdk_pixbuf_query_loaders"},
		{"type":"configure_gcc_runtime"},
		{"type":"symlink","sudo":true,"source":{"path":"a"},"target":{"path":"b"}},
		{"type":"gdk_pixbuf_query_loaders"},
		{"type":"run","command":{"path":"x"}}
	]`))
	want := []string{"gdk_pixbuf_query_loaders", "configure_gcc_runtime", "symlink with sudo"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Unsupported = %v, want %v", got, want)
	}
	if got := Unsupported(raw(`[{"type":"touch","path":{"path":"x"}},{"type":"warn","message":"m"}]`)); len(got) != 0 {
		t.Errorf("Unsupported = %v for supported steps", got)
	}
	if err := (&fixture{}).runUnsupported(); err == nil {
		t.Error("Run should refuse an unsupported step")
	}
}

func (f *fixture) runUnsupported() error {
	return Run(Context{}, []json.RawMessage{json.RawMessage(`{"type":"configure_php"}`)})
}

func TestSetPermissions(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/pkg/1.2.3-1/lib/a/one", "x", 0o444)
	f.write("Cellar/pkg/1.2.3-1/lib/a/b/two", "x", 0o444)
	f.write("Cellar/pkg/1.2.3-1/lib/three", "x", 0o444)
	mode := func(rel string) os.FileMode {
		fi, err := os.Stat(filepath.Join(f.prefix, rel))
		if err != nil {
			t.Fatal(err)
		}
		return fi.Mode().Perm()
	}
	f.mustRun(`[{"type":"set_permissions","permissions":"u+w","non_recursive":true,"paths":[{"base":"lib","path":"a/**/*"},{"base":"lib","path":"missing/*"}]}]`)
	if mode("Cellar/pkg/1.2.3-1/lib/a/one")&0o200 == 0 || mode("Cellar/pkg/1.2.3-1/lib/a/b/two")&0o200 == 0 {
		t.Error("the files matched by ** are not writable")
	}
	if mode("Cellar/pkg/1.2.3-1/lib/three")&0o200 != 0 {
		t.Error("a file outside the glob changed")
	}
	if err := os.Chmod(filepath.Join(f.prefix, "Cellar/pkg/1.2.3-1/lib/a/b/two"), 0o666); err != nil {
		t.Fatal(err)
	}
	f.mustRun(`[{"type":"set_permissions","permissions":"go-w","paths":[{"base":"lib","path":"a"}]}]`)
	if mode("Cellar/pkg/1.2.3-1/lib/a/b/two")&0o022 != 0 {
		t.Error("the recursive change did not reach the files below")
	}
	if err := f.run(`[{"type":"set_permissions","permissions":"u+w","paths":[{"path":"/etc/hosts"}]}]`); err == nil {
		t.Error("changing permissions outside the prefix should be refused")
	}
}

func TestRun(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/pkg/1.2.3-1/libexec/post-install", "#!/bin/sh\nprintf '%s|%s|%s' \"$1\" \"$2\" \"$GREETING\" > \"$2\".out\n", 0o755)
	f.mkdir("etc/pkg")
	f.mustRun(`[{"type":"run","command":{"base":"libexec","path":"post-install"},
		"args":["{{pkgshare}}/in","{{pkgetc}}/cert"],"env":{"GREETING":"hi-{{formula_name}}"}}]`)
	want := filepath.Join(f.prefix, "Cellar/pkg/1.2.3-1/share/pkg/in") + "|" + filepath.Join(f.prefix, "etc/pkg/cert") + "|hi-pkg"
	if got := f.read("etc/pkg/cert.out"); got != want {
		t.Errorf("script output = %q, want %q", got, want)
	}
}

func TestRunFailureAndOptions(t *testing.T) {
	f := newFixture(t)
	f.write("Cellar/pkg/1.2.3-1/libexec/fail", "#!/bin/sh\necho oops >&2\necho out\nexit 3\n", 0o755)
	f.write("Cellar/pkg/1.2.3-1/libexec/cat", "#!/bin/sh\ncat\npwd\n", 0o755)
	err := f.run(`[{"type":"run","command":{"base":"libexec","path":"fail"}}]`)
	if err == nil || !strings.Contains(err.Error(), "oops") {
		t.Errorf("a failing command should fail the step and show its error output: %v", err)
	}
	f.mustRun(`[{"type":"run","allow_failure":true,"command":{"base":"libexec","path":"fail"}}]`)
	f.write("var/in.txt", "from a file\n", 0o644)
	f.mkdir("var/work")
	f.mustRun(`[{"type":"run","command":{"base":"libexec","path":"cat"},"stdin_path":{"base":"var","path":"in.txt"},"chdir":{"base":"var","path":"work"},"stdout_path":{"base":"var","path":"captured/out.txt"}}]`)
	got := f.read("var/captured/out.txt")
	resolved, _ := filepath.EvalSymlinks(filepath.Join(f.prefix, "var/work"))
	if got != "from a file\n"+resolved+"\n" {
		t.Errorf("captured %q", got)
	}
	// A bare command name is found on the PATH.
	f.mustRun(`[{"type":"run","command":{"path":"true"}}]`)
	if err := f.run(`[{"type":"run","command":{"path":"false"}}]`); err == nil {
		t.Error("false should fail")
	}
	if err := f.run(`[{"type":"run","command":{"path":"no-such-command-xyz"}}]`); err == nil {
		t.Error("a missing command should fail")
	}
}

func TestGlobsFollowRubyRules(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"a.txt", "b.rb", ".hidden", "sub/c.txt", "sub/.dot", "sub/deep/d.txt", ".dir/e.txt"} {
		f.write("var/"+n, "x", 0o644)
	}
	removedBy := func(pattern string) []string {
		g := newFixture(t)
		for _, n := range []string{"a.txt", "b.rb", ".hidden", "sub/c.txt", "sub/.dot", "sub/deep/d.txt", ".dir/e.txt"} {
			g.write("var/"+n, "x", 0o644)
		}
		g.mustRun(`[{"type":"remove","paths":[{"base":"var","path":"` + pattern + `"}]}]`)
		var gone []string
		for _, n := range []string{"a.txt", "b.rb", ".hidden", "sub/c.txt", "sub/.dot", "sub/deep/d.txt", ".dir/e.txt"} {
			if !g.exists("var/" + n) {
				gone = append(gone, n)
			}
		}
		return gone
	}
	for pattern, want := range map[string][]string{
		"*":            {"a.txt", "b.rb"},
		".*":           {".hidden"},
		"*.txt":        {"a.txt"},
		"?.rb":         {"b.rb"},
		"*.{txt,rb}":   {"a.txt", "b.rb"},
		"[ab].*":       {"a.txt", "b.rb"},
		"sub/*":        {"sub/c.txt"},
		"**/*.txt":     {"a.txt", "sub/c.txt", "sub/deep/d.txt"},
		"sub/**/*.txt": {"sub/c.txt", "sub/deep/d.txt"},
		"missing/*":    nil,
	} {
		if got := removedBy(pattern); !reflect.DeepEqual(got, want) {
			t.Errorf("pattern %q matched %v, want %v", pattern, got, want)
		}
	}
}

// fakeInitialiser puts a script in the keg's bin that records its arguments and
// whether TMPDIR was set.
func (f *fixture) fakeInitialiser(name string) string {
	f.t.Helper()
	out := filepath.Join(f.prefix, "calls-"+name)
	f.write("Cellar/pkg/1.2.3-1/bin/"+name, "#!/bin/sh\nprintf '%s\\n' \"$@\" > '"+out+"'\nprintf 'TMPDIR=%s\\n' \"${TMPDIR-unset}\" >> '"+out+"'\n", 0o755)
	return out
}

func TestInitDataDirPostgres(t *testing.T) {
	f := newFixture(t)
	f.fakeInitialiser("initdb")
	t.Setenv("TMPDIR", "/kept")
	f.mustRun(`[{"type":"init_data_dir","using":"postgresql_initdb","path":{"base":"var","path":"{{formula_name}}"}}]`)
	want := "--locale=en_US.UTF-8\n-E\nUTF-8\n" + filepath.Join(f.prefix, "var/pkg") + "\nTMPDIR=/kept\n"
	if got := f.read("calls-initdb"); got != want {
		t.Errorf("initdb saw %q, want %q", got, want)
	}
}

func TestInitDataDirPostgresLocale(t *testing.T) {
	f := newFixture(t)
	f.fakeInitialiser("initdb")
	f.mustRun(`[{"type":"init_data_dir","using":"postgresql_initdb","locale":"C","path":{"base":"var","path":"pkg"}}]`)
	if got := f.read("calls-initdb"); !strings.HasPrefix(got, "--locale=C\n") {
		t.Errorf("initdb arguments = %q", got)
	}
}

func TestInitDataDirMySQLAndMariaDB(t *testing.T) {
	for using, program := range map[string]string{"mysql_initialize": "mysqld", "mariadb_install_db": "mysql_install_db"} {
		f := newFixture(t)
		f.fakeInitialiser(program)
		t.Setenv("TMPDIR", "/should/not/be/seen")
		f.mustRun(`[{"type":"init_data_dir","using":"` + using + `","path":{"base":"var","path":"mysql"}}]`)
		got := f.read("calls-" + program)
		keg, data := filepath.Join(f.prefix, "Cellar/pkg/1.2.3-1"), filepath.Join(f.prefix, "var/mysql")
		for _, want := range []string{"--user=tester\n", "--basedir=" + keg + "\n", "--datadir=" + data + "\n", "--tmpdir=/tmp\n", "TMPDIR=unset\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: %q is missing from %q", using, want, got)
			}
		}
		if using == "mysql_initialize" && !strings.HasPrefix(got, "--initialize-insecure\n") {
			t.Errorf("mysqld arguments = %q", got)
		}
		if using == "mariadb_install_db" && !strings.HasPrefix(got, "--verbose\n") {
			t.Errorf("mysql_install_db arguments = %q", got)
		}
	}
}

func TestInitDataDirKeepsAnInitialisedDirectory(t *testing.T) {
	for using, marker := range map[string]string{
		"postgresql_initdb": "var/data/PG_VERSION", "mysql_initialize": "var/data/mysql/general_log.CSM", "mariadb_install_db": "var/data/mysql/user.frm",
	} {
		f := newFixture(t)
		for _, p := range []string{"initdb", "mysqld", "mysql_install_db"} {
			f.fakeInitialiser(p)
		}
		f.write(marker, "", 0o644)
		f.mustRun(`[{"type":"init_data_dir","using":"` + using + `","path":{"base":"var","path":"data"}}]`)
		for _, p := range []string{"initdb", "mysqld", "mysql_install_db"} {
			if f.exists("calls-" + p) {
				t.Errorf("%s: %s ran although %s exists", using, p, marker)
			}
		}
	}
}

func TestInitDataDirCreatesThePathAndSkipsOnGitHubActions(t *testing.T) {
	f := newFixture(t)
	f.fakeInitialiser("initdb")
	t.Setenv("HOMEBREW_GITHUB_ACTIONS", "true")
	f.mustRun(`[{"type":"init_data_dir","using":"postgresql_initdb","path":{"base":"var","path":"deep/data"}}]`)
	if !f.exists("var/deep/data") || f.exists("calls-initdb") {
		t.Errorf("directory exists = %v, initdb ran = %v", f.exists("var/deep/data"), f.exists("calls-initdb"))
	}
}

func TestInitDataDirFailures(t *testing.T) {
	f := newFixture(t)
	if err := f.run(`[{"type":"init_data_dir","using":"sqlite","path":{"base":"var","path":"d"}}]`); err == nil {
		t.Error("an unknown initialiser should fail")
	}
	f.write("Cellar/pkg/1.2.3-1/bin/initdb", "#!/bin/sh\necho boom >&2\nexit 3\n", 0o755)
	err := f.run(`[{"type":"init_data_dir","using":"postgresql_initdb","path":{"base":"var","path":"d"}}]`)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("a failing initdb should report its error, got %v", err)
	}
	f.ctx.User = ""
	f.fakeInitialiser("mysqld")
	if err := f.run(`[{"type":"init_data_dir","using":"mysql_initialize","path":{"base":"var","path":"m"}}]`); err == nil {
		t.Error("mysql needs a user name")
	}
}
