package poststep

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/sudo"
)

func (f *sudoFixture) spec(p string) string { return fmt.Sprintf(`{"path":%q}`, p) }

func (f *sudoFixture) tree(name string, files map[string]string) string {
	f.t.Helper()
	root := filepath.Join(f.dir, name)
	if err := os.MkdirAll(root, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	return root
}

func (f *sudoFixture) slurp(p string) string {
	f.t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestMoveContentsMovesTheChildrenAndSkipsTheTarget(t *testing.T) {
	for _, typ := range []string{"move_children", "move_contents"} {
		t.Run(typ, func(t *testing.T) {
			f := newSudoFixture(t)
			src := f.tree("src", map[string]string{"a": "A", "sub/b": "B"})
			target := filepath.Join(src, "sub")
			f.mustRun(fmt.Sprintf(`[{"type":%q,"source":%s,"target":%s}]`, typ, f.spec(src), f.spec(target)))
			if f.slurp(filepath.Join(target, "a")) != "A" || f.slurp(filepath.Join(target, "b")) != "B" {
				t.Error("children were not moved into the target")
			}
			if _, err := os.Lstat(filepath.Join(src, "a")); err == nil {
				t.Error("the child stayed in the source")
			}
			if _, err := os.Lstat(target); err != nil {
				t.Error("the target must not move into itself")
			}
		})
	}
}

func TestMoveContentsMakesTheTargetAndIgnoresAnEmptySource(t *testing.T) {
	f := newSudoFixture(t)
	src := f.tree("empty", nil)
	target := filepath.Join(f.dir, "made/deep")
	f.mustRun(fmt.Sprintf(`[{"type":"move_contents","source":%s,"target":%s}]`, f.spec(src), f.spec(target)))
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		t.Errorf("the target was not made: %v", err)
	}
}

func TestInstallGzippedExecutable(t *testing.T) {
	f := newSudoFixture(t)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	_, _ = w.Write([]byte("#!/bin/sh\n"))
	_ = w.Close()
	src := filepath.Join(f.dir, "tool.gz")
	if err := os.WriteFile(src, gz.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.dir, "bin/tool")
	step := fmt.Sprintf(`[{"type":"install_gzipped_executable","source":%s,"target":%s}]`, f.spec(src), f.spec(target))
	f.mustRun(step)
	fi, err := os.Stat(target)
	if err != nil || f.slurp(target) != "#!/bin/sh\n" || fi.Mode().Perm() != 0o755 {
		t.Errorf("target = %v %v", fi, err)
	}
	if _, err := os.Lstat(src); err == nil {
		t.Error("the archive was not removed")
	}
	if entries, _ := os.ReadDir(filepath.Dir(target)); len(entries) != 1 {
		t.Errorf("a temporary file stayed behind: %v", entries)
	}
	f.mustRun(step)
}

func TestInstallGzippedExecutableLeavesTheTargetWhenTheArchiveIsBad(t *testing.T) {
	f := newSudoFixture(t)
	src := f.touch("bad.gz")
	target := filepath.Join(f.dir, "tool")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.run(fmt.Sprintf(`[{"type":"install_gzipped_executable","source":%s,"target":%s}]`, f.spec(src), f.spec(target))); err == nil {
		t.Fatal("a bad archive should fail")
	}
	if f.slurp(target) != "old" {
		t.Error("the old target was replaced")
	}
	if _, err := os.Lstat(filepath.Join(f.dir, ".tool.install-step")); err == nil {
		t.Error("the temporary file stayed behind")
	}
}

func (f *sudoFixture) inreplace(file, extra string) error {
	return f.run(fmt.Sprintf(`[{"type":"inreplace","path":%s%s}]`, f.spec(file), extra))
}

func TestInreplaceReplacesEveryMatchUnlessFirstOnly(t *testing.T) {
	f := newSudoFixture(t)
	file := f.touch("conf")
	if err := os.WriteFile(file, []byte("a=1 a=1 a=1"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o640); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"inreplace","path":%s,"before":"a=1","after":"z={{version}}","first_only":true}]`, f.spec(file)))
	if got := f.slurp(file); got != "z=1.2.3 a=1 a=1" {
		t.Errorf("first only: %q", got)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"inreplace","path":%s,"before":"a=1","after":"b"}]`, f.spec(file)))
	if got := f.slurp(file); got != "z=1.2.3 b b" {
		t.Errorf("global: %q", got)
	}
	if fi, _ := os.Stat(file); fi.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want it kept", fi.Mode())
	}
}

func TestInreplaceRegexp(t *testing.T) {
	f := newSudoFixture(t)
	file := f.touch("conf")
	if err := os.WriteFile(file, []byte("Name=old\nname=OLD\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"inreplace","path":%s,"regexp":true,"regexp_options":1,"before":"^(name)=\\w+$","after":"\\1=new"}]`, f.spec(file)))
	if got := f.slurp(file); got != "Name=new\nname=new\n" {
		t.Errorf("got %q", got)
	}
}

func TestInreplaceStringsTakeNoRegexpMeaning(t *testing.T) {
	f := newSudoFixture(t)
	file := f.touch("conf")
	if err := os.WriteFile(file, []byte("a.c abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"inreplace","path":%s,"before":"a.c","after":"x"}]`, f.spec(file)))
	if got := f.slurp(file); got != "x abc" {
		t.Errorf("got %q", got)
	}
}

func TestInreplaceFailsWithBrewsMessageWhenNothingMatches(t *testing.T) {
	f := newSudoFixture(t)
	file := f.touch("conf")
	err := f.inreplace(file, `,"before":"nope","after":"x"`)
	want := "inreplace failed\n" + file + ":\n  expected replacement of \"nope\" with \"x\"\n"
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("err = %q, want it to end with %q", err, want)
	}
	err = f.inreplace(file, `,"regexp":true,"regexp_options":5,"before":"n.pe","after":"x"`)
	want = "  expected replacement of /n.pe/mi with \"x\"\n"
	if err == nil || !strings.HasSuffix(err.Error(), want) {
		t.Errorf("err = %q, want it to end with %q", err, want)
	}
	if err := f.inreplace(file, `,"before":"nope","after":"x","skip_audit":true`); err != nil {
		t.Errorf("an unaudited miss is fine: %v", err)
	}
}

func TestInreplaceOfAMissingFileFails(t *testing.T) {
	f := newSudoFixture(t)
	if err := f.inreplace(filepath.Join(f.dir, "missing"), `,"before":"a","after":"b"`); err == nil {
		t.Error("a missing file should fail")
	}
}

func TestSetOwnershipRunsChownAsTheUserFirst(t *testing.T) {
	f := newSudoFixture(t)
	t.Setenv("HOMEBREW_NO_SUDO", "")
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	app := f.tree("My.app", map[string]string{"x": "x"})
	f.mustRun(fmt.Sprintf(`[{"type":"set_ownership","paths":[%s,%s]}]`, f.spec(app), f.spec(filepath.Join(f.dir, "missing"))))
	c := f.only()
	if c.Exe != "chown" || c.Privilege != sudo.User || !c.MustSucceed {
		t.Errorf("runner got %v, want chown as the user", c)
	}
	owner := currentUserName(t)
	if want := []string{"-R", "--", owner + ":staff", app}; !slices.Equal(c.Args, want) {
		t.Errorf("args = %q, want %q", c.Args, want)
	}
	if !strings.Contains(f.log.String(), "==> Changing ownership of paths required by pkg...") {
		t.Errorf("log = %q", f.log.String())
	}
}

func TestSetOwnershipTakesUserGroupAndNonRecursive(t *testing.T) {
	f := newSudoFixture(t)
	app := f.tree("My.app", nil)
	f.mustRun(fmt.Sprintf(`[{"type":"set_ownership","user":"bob","group":"wheel","non_recursive":true,"paths":[%s]}]`, f.spec(app)))
	if want := []string{"--", "bob:wheel", app}; !slices.Equal(f.only().Args, want) {
		t.Errorf("args = %q, want %q", f.only().Args, want)
	}
}

func TestSetOwnershipRetriesWithSudoWhenTheUserCannot(t *testing.T) {
	f := newSudoFixture(t)
	app := f.tree("My.app", nil)
	t.Setenv("HOMEBREW_NO_SUDO", "")
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	failure := commandFailure(t)
	calls := 0
	f.ctx.Sudo = func(_ context.Context, c sudo.Command) (sudo.Result, error) {
		f.stub.cmds = append(f.stub.cmds, c)
		calls++
		if calls == 1 {
			return sudo.Result{}, failure
		}
		return sudo.Result{}, nil
	}
	f.mustRun(fmt.Sprintf(`[{"type":"set_ownership","paths":[%s]}]`, f.spec(app)))
	if len(f.stub.cmds) != 2 || f.stub.cmds[0].Privilege != sudo.User || f.stub.cmds[1].Privilege != sudo.Sudo {
		t.Errorf("commands = %v, want the user and then sudo", f.stub.cmds)
	}
	if !slices.Equal(f.stub.cmds[0].Args, f.stub.cmds[1].Args) {
		t.Error("the retry changed the arguments")
	}
}

func TestSetOwnershipStaysFailedWhenSudoIsDisabled(t *testing.T) {
	f := newSudoFixture(t)
	app := f.tree("My.app", nil)
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	f.stub.err = commandFailure(t)
	if err := f.run(fmt.Sprintf(`[{"type":"set_ownership","paths":[%s]}]`, f.spec(app))); err == nil {
		t.Error("a failed chown without sudo should fail")
	}
	if len(f.stub.cmds) != 1 {
		t.Errorf("commands = %v, want only the user's attempt", f.stub.cmds)
	}
}

func TestSetOwnershipNeedsAppManagementPermissions(t *testing.T) {
	f := newSudoFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	t.Setenv("HOMEBREW_NO_SUDO", "")
	t.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	app := f.tree("Locked.app", nil)
	if err := os.Chmod(app, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(app, 0o755) })
	probe := filepath.Join(app, ".homebrew-write-test")
	f.ctx.Sudo = func(_ context.Context, c sudo.Command) (sudo.Result, error) {
		f.stub.cmds = append(f.stub.cmds, c)
		if c.Exe == "touch" {
			return sudo.Result{Stderr: "touch: " + probe + ": Operation not permitted\n"}, fmt.Errorf("touch failed: touch: %s: Operation not permitted", probe)
		}
		return sudo.Result{}, nil
	}
	err := f.run(fmt.Sprintf(`[{"type":"set_ownership","paths":[%s]}]`, f.spec(app)))
	if err == nil || !strings.Contains(err.Error(), "Cannot change the ownership of '"+app+"' because your terminal does not have App Management permissions.") {
		t.Fatalf("err = %v", err)
	}
	for _, c := range f.stub.cmds {
		if c.Exe == "chown" {
			t.Error("chown ran without the permissions")
		}
	}
}

func TestSetOwnershipProbeUsesTheUserFirstAndSudoOnFailureOfSomethingElse(t *testing.T) {
	f := newSudoFixture(t)
	if os.Geteuid() == 0 {
		t.Skip("root can write to a read-only directory")
	}
	t.Setenv("HOMEBREW_NO_SUDO", "1")
	app := f.tree("Locked.app", nil)
	if err := os.Chmod(app, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(app, 0o755) })
	f.stub.err = commandFailure(t)
	err := f.run(fmt.Sprintf(`[{"type":"set_ownership","paths":[%s]}]`, f.spec(app)))
	if err == nil || strings.Contains(err.Error(), "App Management") {
		t.Errorf("a touch failure that is not the permission one must propagate as it is: %v", err)
	}
}

func currentUserName(t *testing.T) string {
	t.Helper()
	name, err := currentUser()
	if err != nil {
		t.Fatal(err)
	}
	return name
}

func (f *sudoFixture) opt(formula, exe string) string {
	f.t.Helper()
	p := filepath.Join(f.prefix, "opt", formula, "bin", exe)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, nil, 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func TestFormulaToolSteps(t *testing.T) {
	dir := func(f *sudoFixture) string { return filepath.Join(f.dir, "share") }
	for _, tt := range []struct {
		typ, formula, exe string
		args              func(path string) []string
	}{
		{"compile_gsettings_schemas", "glib", "glib-compile-schemas", func(p string) []string { return []string{p} }},
		{"gio_querymodules", "glib", "gio-querymodules", func(p string) []string { return []string{p} }},
		{"update_mime_database", "shared-mime-info", "update-mime-database", func(p string) []string { return []string{p} }},
		{"update_desktop_database", "desktop-file-utils", "update-desktop-database", func(p string) []string { return []string{p} }},
		{"gdk_pixbuf_query_loaders", "gdk-pixbuf", "gdk-pixbuf-query-loaders", func(string) []string { return []string{"--update-cache"} }},
	} {
		t.Run(tt.typ, func(t *testing.T) {
			f := newSudoFixture(t)
			tool := f.opt(tt.formula, tt.exe)
			f.mustRun(fmt.Sprintf(`[{"type":%q,"path":%s}]`, tt.typ, f.spec(dir(f))))
			c := f.only()
			if c.Exe != tool || c.Privilege != sudo.User || !c.MustSucceed || !slices.Equal(c.Args, tt.args(dir(f))) {
				t.Errorf("runner got %v, want %s %v", c, tool, tt.args(dir(f)))
			}
			if c.Stdout != nil && c.Stdout != &f.log {
				t.Errorf("stdout = %v", c.Stdout)
			}
		})
	}
}

func TestFormulaToolStepsNeedTheToolInstalled(t *testing.T) {
	f := newSudoFixture(t)
	err := f.run(fmt.Sprintf(`[{"type":"update_mime_database","path":%s}]`, f.spec(f.dir)))
	want := "shared-mime-info is missing required executable: " + filepath.Join(f.prefix, "opt/shared-mime-info/bin/update-mime-database")
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %q", err, want)
	}
	if len(f.stub.cmds) != 0 {
		t.Errorf("ran %v", f.stub.cmds)
	}
}

func TestGtkIconCachePrefersGtk4WhenAnyVersionIsInstalled(t *testing.T) {
	f := newSudoFixture(t)
	gtk3 := f.opt("gtk+3", "gtk3-update-icon-cache")
	step := fmt.Sprintf(`[{"type":"gtk_update_icon_cache","path":%s}]`, f.spec(f.dir))
	f.mustRun(step)
	if c := f.only(); c.Exe != gtk3 || !slices.Equal(c.Args, []string{"-q", "-t", "-f", f.dir}) {
		t.Errorf("runner got %v", c)
	}
	f.stub.cmds = nil
	gtk4 := f.opt("gtk4", "gtk4-update-icon-cache")
	f.write("Cellar/gtk4/4.0/INSTALL_RECEIPT.json", "{}", 0o644)
	f.mustRun(step)
	if c := f.only(); c.Exe != gtk4 {
		t.Errorf("runner got %v, want gtk4", c)
	}
}

func TestConfigureClangSystemWritesTheConfigFilesOnMacOS(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.MacVersion, f.ctx.Kernel, f.ctx.GOARCH = "27.0.1", "27.1.0", "arm64"
	f.mustRun(`[{"type":"configure_clang_system"}]`)
	dir := filepath.Join(f.prefix, "etc/clang")
	want := "-isysroot /Library/Developer/CommandLineTools/SDKs/MacOSX27.sdk\n"
	for _, name := range []string{"arm64-apple-darwin27.cfg", "x86_64-apple-darwin27.cfg", "aarch64-apple-darwin27.cfg",
		"arm64-apple-macosx27.cfg", "x86_64-apple-macosx27.cfg", "aarch64-apple-macosx27.cfg"} {
		if got := f.slurp(filepath.Join(dir, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestConfigureClangSystemDoesNothingOnLinux(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.OS = "linux"
	f.mustRun(`[{"type":"configure_clang_system"}]`)
	if f.exists("etc/clang") {
		t.Error("the config was written on linux")
	}
}

func TestConfigureGCCRuntimeDoesNothingOnMacOS(t *testing.T) {
	f := newSudoFixture(t)
	f.mustRun(`[{"type":"configure_gcc_runtime"}]`)
	if len(f.stub.cmds) != 0 {
		t.Errorf("ran %v", f.stub.cmds)
	}
}

func TestConfigureGlibcRuntimeInstallsLocalesAndLinksTheSystemFiles(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.Name = "glibc"
	t.Setenv("HOMEBREW_LANG", "de_DE.utf8")
	t.Setenv("LANG", "C")
	t.Setenv("LC_ALL", "C")
	t.Setenv("LC_CTYPE", "C")
	f.mkdir("Cellar/pkg/1.2.3-1/bin")
	f.mustRun(`[{"type":"configure_glibc_runtime"}]`)
	bin := filepath.Join(f.prefix, "Cellar/pkg/1.2.3-1/bin/localedef")
	var got [][]string
	for _, c := range f.stub.cmds {
		if c.Exe != bin {
			t.Errorf("ran %v", c)
		}
		got = append(got, c.Args)
	}
	want := [][]string{{"-i", "de_DE", "-f", "UTF-8", "de_DE.utf8"}, {"-i", "en_US", "-f", "UTF-8", "en_US.UTF-8"}}
	if !slices.EqualFunc(got, want, slices.Equal) {
		t.Errorf("localedef args = %q, want %q", got, want)
	}
	if !f.exists("Cellar/pkg/1.2.3-1/lib/locale") {
		t.Error("the locale directory was not made")
	}
	if !strings.Contains(f.log.String(), "==> Installing locale data for de_DE.utf8 en_US.UTF-8") {
		t.Errorf("log = %q", f.log.String())
	}
}

func TestChangeDylibIDEditsTheIDAndSignsOnAppleSilicon(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.GOARCH = "arm64"
	lib := filepath.Join(f.dir, "libx.dylib")
	if err := os.WriteFile(lib, tinyDylib("/old/libx.dylib"), 0o444); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"change_dylib_id","source":%s,"id":"{{HOMEBREW_PREFIX}}/lib/libx.dylib"}]`, f.spec(lib)))
	if got := f.slurp(lib); !strings.Contains(got, f.prefix+"/lib/libx.dylib") {
		t.Errorf("the id was not changed")
	}
	if fi, _ := os.Stat(lib); fi.Mode().Perm() != 0o444 {
		t.Errorf("mode = %v, want the read-only mode restored", fi.Mode())
	}
	c := f.only()
	if c.Exe != "codesign" || !slices.Equal(c.Args, []string{"--sign", "-", "--force", "--preserve-metadata=entitlements,requirements,flags,runtime", lib}) {
		t.Errorf("runner got %v", c)
	}
}

func TestChangeDylibIDDoesNotSignOnIntel(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.GOARCH = "amd64"
	lib := filepath.Join(f.dir, "libx.dylib")
	if err := os.WriteFile(lib, tinyDylib("/old/libx.dylib"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"change_dylib_id","source":%s,"id":"/new/libx.dylib"}]`, f.spec(lib)))
	if len(f.stub.cmds) != 0 {
		t.Errorf("ran %v", f.stub.cmds)
	}
}

func TestChangeDylibIDResolvesASymlinkSourceWhenAsked(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.GOARCH = "amd64"
	real := filepath.Join(f.dir, "libx.1.dylib")
	if err := os.WriteFile(real, tinyDylib("/old/libx.dylib"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(f.dir, "libx.dylib")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	f.mustRun(fmt.Sprintf(`[{"type":"change_dylib_id","resolve_source":true,"source":%s,"id":"/new/libx.dylib"}]`, f.spec(link)))
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link was replaced by a file: %v", err)
	}
	if !strings.Contains(f.slurp(real), "/new/libx.dylib") {
		t.Error("the target of the link was not edited")
	}
}

func tinyDylib(id string) []byte {
	le := binary.LittleEndian
	size := (24 + len(id) + 1 + 7) &^ 7
	data := make([]byte, 0x400)
	le.PutUint32(data[0:], 0xfeedfacf)
	le.PutUint32(data[4:], 0x0100000c)
	le.PutUint32(data[12:], 6)
	le.PutUint32(data[16:], 1)
	le.PutUint32(data[20:], uint32(size))
	le.PutUint32(data[32:], 0xd)
	le.PutUint32(data[36:], uint32(size))
	le.PutUint32(data[40:], 24)
	copy(data[56:], id)
	return data
}
