package install

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/bottle"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/install/installtest"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
)

var fixedTime = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

type env struct {
	t      *testing.T
	prefix string
	root   *fsx.Root
	reg    *installtest.Registry
	in     *Installer
	signed [][]string
	log    bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	prefix := t.TempDir()
	for _, d := range []string{"Cellar", "opt", "bin", "etc", "include", "lib", "sbin", "share", "var/homebrew/linked", "var/homebrew/locks"} {
		if err := os.MkdirAll(filepath.Join(prefix, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	root, err := fsx.Open(prefix, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	e := &env{t: t, prefix: prefix, root: root, reg: installtest.NewRegistry(t)}
	e.in = &Installer{
		Root: root, Prefix: prefix,
		Fetcher: &bottle.Fetcher{Dir: filepath.Join(t.TempDir(), "downloads")},
		Linker:  &link.Linker{Root: root, Prefix: prefix},
		Now:     func() time.Time { return fixedTime },
		APIPath: "/cache/api/packages.json",
		Log:     &e.log,
		Sign: func(_ *fsx.Root, _ string, files []string) error {
			e.signed = append(e.signed, files)
			return nil
		},
	}
	return e
}

func file(name, body string, mode int64) installtest.File {
	return installtest.File{Name: name, Body: body, Mode: mode}
}

func formulaJSON(t *testing.T, name string, extra map[string]any) *api.Formula {
	t.Helper()
	m := map[string]any{"name": name, "full_name": name, "tap": "homebrew/core", "versions": map[string]any{"stable": "1.0"},
		"aliases": []string{}, "version_scheme": 0, "bottle": map[string]any{"stable": map[string]any{"rebuild": 0}}}
	for k, v := range extra {
		m[k] = v
	}
	b, err := json.Marshal([]any{m})
	if err != nil {
		t.Fatal(err)
	}
	fs, err := api.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return &fs[0]
}

func (e *env) item(spec installtest.Bottle, onRequest, kegOnly bool, extra map[string]any) Item {
	e.t.Helper()
	cellarKind := ":any"
	return Item{
		Formula: formulaJSON(e.t, spec.Name, extra), Tag: "arm64_tahoe", Bottle: e.reg.Add(spec, cellarKind),
		PkgVersion: spec.Version, OnRequest: onRequest, KegOnly: kegOnly,
	}
}

func (e *env) install(items ...Item) ([]Result, error) {
	e.t.Helper()
	return e.in.Install(context.Background(), &Plan{Items: items})
}

func (e *env) read(rel string) string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.prefix, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

func (e *env) exists(rel string) bool {
	_, err := os.Lstat(filepath.Join(e.prefix, rel))
	return err == nil
}

func (e *env) readlink(rel string) string {
	e.t.Helper()
	target, err := os.Readlink(filepath.Join(e.prefix, rel))
	if err != nil {
		e.t.Fatal(err)
	}
	return target
}

var jq = installtest.Bottle{
	Name: "jq", Version: "1.0",
	Files: []installtest.File{
		file("bin/jq", "#!/bin/sh\necho jq", 0o755),
		file("lib/pkgconfig/jq.pc", "prefix=@@HOMEBREW_CELLAR@@/jq/1.0\nlibdir=@@HOMEBREW_PREFIX@@/lib\n", 0),
		file("share/man/man1/jq.1", "man page", 0),
	},
	Changed: []string{"lib/pkgconfig/jq.pc"},
}

func TestInstallPoursRelocatesAndLinks(t *testing.T) {
	e := newEnv(t)
	results, err := e.install(e.item(jq, true, false, map[string]any{"aliases": []string{"jaq"}}))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Name != "jq" || results[0].Version != "1.0" || !results[0].Linked || results[0].LinkErr != nil {
		t.Fatalf("results = %+v", results)
	}
	if got := e.read("Cellar/jq/1.0/lib/pkgconfig/jq.pc"); got != "prefix="+e.prefix+"/Cellar/jq/1.0\nlibdir="+e.prefix+"/lib\n" {
		t.Errorf("pkgconfig file = %q", got)
	}
	if e.read("Cellar/jq/1.0/bin/jq") != "#!/bin/sh\necho jq" {
		t.Error("the binary changed")
	}
	if fi, _ := os.Stat(filepath.Join(e.prefix, "Cellar/jq/1.0/bin/jq")); fi.Mode().Perm() != 0o755 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	for link, target := range map[string]string{
		"bin/jq":                 "../Cellar/jq/1.0/bin/jq",
		"opt/jq":                 "../Cellar/jq/1.0",
		"opt/jaq":                "../Cellar/jq/1.0",
		"var/homebrew/linked/jq": "../../../Cellar/jq/1.0",
		"share/man/man1/jq.1":    "../../../Cellar/jq/1.0/share/man/man1/jq.1",
		"lib/pkgconfig/jq.pc":    "../../Cellar/jq/1.0/lib/pkgconfig/jq.pc",
	} {
		if got := e.readlink(link); got != target {
			t.Errorf("%s -> %s, want %s", link, got, target)
		}
	}
	if e.exists(fsx.Staging) {
		entries, _ := os.ReadDir(filepath.Join(e.prefix, fsx.Staging))
		if len(entries) != 0 {
			t.Errorf("staging left behind: %v", entries)
		}
	}
}

func TestInstallWritesTheReceiptBrewWould(t *testing.T) {
	e := newEnv(t)
	dep := installtest.Bottle{Name: "libx", Version: "2.0", Files: []installtest.File{file("lib/libx.a", "x", 0)}}
	app := installtest.Bottle{Name: "app", Version: "1.0", Files: []installtest.File{file("bin/app", "app", 0o755)}, Deps: []string{"libx"}}
	if _, err := e.install(e.item(dep, false, false, nil), e.item(app, true, false, map[string]any{"aliases": []string{"a"}, "version_scheme": 2})); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name, version string
		onRequest     bool
		aliases       []string
	}{{"libx", "2.0", false, []string{}}, {"app", "1.0", true, []string{"a"}}} {
		r, err := cellar.ParseReceipt([]byte(e.read("Cellar/" + tt.name + "/" + tt.version + "/INSTALL_RECEIPT.json")))
		if err != nil {
			t.Fatal(err)
		}
		if !r.PouredFromBottle || !r.BuiltAsBottle || !r.LoadedFromAPI || r.InstalledOnRequest != tt.onRequest || r.Time != fixedTime.Unix() {
			t.Errorf("%s receipt = %+v", tt.name, r)
		}
		if !reflect.DeepEqual(r.Aliases, tt.aliases) || r.Source.Tap != "homebrew/core" || r.HomebrewVersion != "6.0.22" {
			t.Errorf("%s: aliases %v, tap %q, homebrew_version %q", tt.name, r.Aliases, r.Source.Tap, r.HomebrewVersion)
		}
		if _, err := r.Marshal(); err != nil {
			t.Errorf("%s: %v", tt.name, err)
		}
	}
	r, _ := cellar.ParseReceipt([]byte(e.read("Cellar/app/1.0/INSTALL_RECEIPT.json")))
	if r.VersionScheme() != 2 || len(r.RuntimeDependencies) != 1 || r.RuntimeDependencies[0].FullName != "libx" {
		t.Errorf("scheme %d, deps %+v", r.VersionScheme(), r.RuntimeDependencies)
	}
	if !strings.Contains(e.read("Cellar/app/1.0/INSTALL_RECEIPT.json"), `"path": "/cache/api/packages.json"`) {
		t.Error("the receipt does not name the API path")
	}
}

func TestInstallKegOnlyFormulaeGetOnlyAnOptLink(t *testing.T) {
	e := newEnv(t)
	results, err := e.install(e.item(jq, true, true, nil))
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Linked {
		t.Error("a keg-only formula reported as linked")
	}
	if e.readlink("opt/jq") != "../Cellar/jq/1.0" {
		t.Error("no opt record")
	}
	for _, p := range []string{"bin/jq", "var/homebrew/linked/jq", "lib/pkgconfig"} {
		if e.exists(p) {
			t.Errorf("%s exists for a keg-only formula", p)
		}
	}
}

// machO builds a thin arm64 dylib whose load commands name placeholders.
func machO(name string) string {
	data := make([]byte, 0x1000)
	binary.LittleEndian.PutUint32(data[0:], 0xfeedfacf)
	binary.LittleEndian.PutUint32(data[12:], 6) // dylib
	cmd := make([]byte, (24+len(name)+1+7)&^7)
	binary.LittleEndian.PutUint32(cmd[0:], 0xc) // LC_LOAD_DYLIB
	binary.LittleEndian.PutUint32(cmd[4:], uint32(len(cmd)))
	binary.LittleEndian.PutUint32(cmd[8:], 24)
	copy(cmd[24:], name)
	copy(data[32:], cmd)
	binary.LittleEndian.PutUint32(data[16:], 1)
	binary.LittleEndian.PutUint32(data[20:], uint32(len(cmd)))
	return string(data)
}

func TestInstallRelocatesAndSignsMachOFiles(t *testing.T) {
	e := newEnv(t)
	spec := installtest.Bottle{Name: "tool", Version: "1.0", Files: []installtest.File{file("bin/tool", machO("@@HOMEBREW_PREFIX@@/opt/dep/lib/libdep.dylib"), 0o755)}, Linkage: []string{"bin/tool"}}
	if _, err := e.install(e.item(spec, true, false, nil)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.read("Cellar/tool/1.0/bin/tool"), e.prefix+"/opt/dep/lib/libdep.dylib") {
		t.Error("the library name was not relocated")
	}
	if !reflect.DeepEqual(e.signed, [][]string{{"bin/tool"}}) {
		t.Errorf("signed = %v", e.signed)
	}
}

func TestInstallSkipsMachORelocationForBottlesThatNeedNone(t *testing.T) {
	e := newEnv(t)
	spec := installtest.Bottle{Name: "tool", Version: "1.0", Files: []installtest.File{file("bin/tool", machO("@@HOMEBREW_PREFIX@@/x"), 0o755), file("share/t.txt", "@@HOMEBREW_PREFIX@@/etc", 0)}, Changed: []string{"share/t.txt"}, Linkage: []string{"bin/tool"}}
	item := e.item(spec, true, false, nil)
	item.Bottle.Cellar = ":any_skip_relocation"
	if _, err := e.install(item); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.read("Cellar/tool/1.0/bin/tool"), "@@HOMEBREW_PREFIX@@/x") || len(e.signed) != 0 {
		t.Error("a bottle that needs no relocation had its binary rewritten or signed")
	}
	if e.read("Cellar/tool/1.0/share/t.txt") != e.prefix+"/etc" {
		t.Error("text files are still relocated")
	}
}

func TestInstallLinkConflictLeavesAPouredUnlinkedKeg(t *testing.T) {
	e := newEnv(t)
	if err := os.WriteFile(filepath.Join(e.prefix, "bin/jq"), []byte("the user's"), 0o755); err != nil {
		t.Fatal(err)
	}
	results, err := e.install(e.item(jq, true, false, nil))
	if err != nil {
		t.Fatalf("a link conflict must not fail the install: %v", err)
	}
	var conflict *link.ConflictError
	if results[0].Linked || !errors.As(results[0].LinkErr, &conflict) {
		t.Fatalf("result = %+v", results[0])
	}
	if !e.exists("Cellar/jq/1.0/bin/jq") || e.readlink("opt/jq") != "../Cellar/jq/1.0" {
		t.Error("the keg or its opt record is missing")
	}
	if e.exists("var/homebrew/linked/jq") || e.read("bin/jq") != "the user's" {
		t.Error("a failed link left a record or touched the user's file")
	}
}

func TestInstallFailureBeforeTheRenameLeavesNothing(t *testing.T) {
	e := newEnv(t)
	good := e.item(jq, true, false, nil)
	e.reg.Tamper(good.Bottle) // the registry serves other bytes than the checksum says
	if _, err := e.install(good); err == nil {
		t.Fatal("expected a checksum error")
	}
	for _, p := range []string{"Cellar/jq", "opt/jq", "bin/jq", "var/homebrew/linked/jq"} {
		if e.exists(p) {
			t.Errorf("%s exists after a failed install", p)
		}
	}
}

func TestInstallBadArchiveLeavesNothing(t *testing.T) {
	e := newEnv(t)
	bad := installtest.Bottle{Name: "evil", Version: "1.0", Files: []installtest.File{file("../../escape", "x", 0)}}
	item := e.item(bad, true, false, nil)
	_, err := e.install(item)
	if err == nil {
		t.Fatal("expected an error")
	}
	if e.exists("Cellar/evil") || e.exists("escape") {
		t.Error("an unsafe archive left files behind")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.prefix, fsx.Staging)); len(entries) != 0 {
		t.Errorf("staging left behind: %v", entries)
	}
}

func TestInstallNeverReplacesAnExistingKeg(t *testing.T) {
	e := newEnv(t)
	if err := os.MkdirAll(filepath.Join(e.prefix, "Cellar/jq/1.0/bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.prefix, "Cellar/jq/1.0/bin/jq"), []byte("existing"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.install(e.item(jq, true, false, nil)); !errors.Is(err, fsx.ErrExists) {
		t.Errorf("error = %v, want ErrExists", err)
	}
	if e.read("Cellar/jq/1.0/bin/jq") != "existing" {
		t.Error("the existing keg was changed")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.prefix, fsx.Staging)); len(entries) != 0 {
		t.Errorf("staging left behind: %v", entries)
	}
}

func TestInstallStopsAtTheFirstFailureAndKeepsWhatWasInstalled(t *testing.T) {
	e := newEnv(t)
	first := e.item(installtest.Bottle{Name: "first", Version: "1.0", Files: []installtest.File{file("bin/first", "1", 0o755)}}, true, false, nil)
	second := e.item(installtest.Bottle{Name: "second", Version: "1.0", Files: []installtest.File{file("../../x", "2", 0)}}, true, false, nil)
	third := e.item(installtest.Bottle{Name: "third", Version: "1.0", Files: []installtest.File{file("bin/third", "3", 0o755)}}, true, false, nil)
	results, err := e.install(first, second, third)
	if err == nil {
		t.Fatal("expected an error")
	}
	if len(results) != 1 || results[0].Name != "first" {
		t.Errorf("results = %+v, want only the first", results)
	}
	if !e.exists("Cellar/first/1.0") || !e.exists("bin/first") {
		t.Error("the first formula should stay installed")
	}
	if e.exists("Cellar/second") || e.exists("Cellar/third") {
		t.Error("nothing after the failure should be installed")
	}
}

func TestInstallLocksTheFormulae(t *testing.T) {
	e := newEnv(t)
	held, err := lock.Acquire(e.root, "jq")
	if err != nil {
		t.Fatal(err)
	}
	item := e.item(jq, true, false, nil)
	before := e.reg.ManifestRequests()
	if _, err := e.install(item); !errors.Is(err, lock.ErrInProgress) {
		t.Errorf("error = %v, want ErrInProgress", err)
	}
	if e.exists("Cellar/jq") || e.reg.ManifestRequests() != before {
		t.Error("a locked formula was downloaded or installed")
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.install(item); err != nil {
		t.Fatalf("after the lock was released: %v", err)
	}
	// The install let go of its own lock.
	again, err := lock.Acquire(e.root, "jq")
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	_ = again.Release()
}

func TestInstallLocksAllOrNothing(t *testing.T) {
	e := newEnv(t)
	held, err := lock.Acquire(e.root, "b")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Release() }()
	a := e.item(installtest.Bottle{Name: "a", Version: "1.0", Files: []installtest.File{file("bin/a", "a", 0o755)}}, true, false, nil)
	b := e.item(installtest.Bottle{Name: "b", Version: "1.0", Files: []installtest.File{file("bin/b", "b", 0o755)}}, true, false, nil)
	if _, err := e.install(a, b); !errors.Is(err, lock.ErrInProgress) {
		t.Fatalf("error = %v", err)
	}
	if e.exists("Cellar/a") {
		t.Error("a was installed although b could not be locked")
	}
	if got, err := lock.Acquire(e.root, "a"); err != nil {
		t.Errorf("the lock on a was left held: %v", err)
	} else {
		_ = got.Release()
	}
}

func TestInstallCancelled(t *testing.T) {
	e := newEnv(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.in.Install(ctx, &Plan{Items: []Item{e.item(jq, true, false, nil)}}); err == nil {
		t.Error("expected an error")
	}
	if e.exists("Cellar/jq") {
		t.Error("a cancelled install changed the Cellar")
	}
}

func TestInstallNothing(t *testing.T) {
	e := newEnv(t)
	results, err := e.in.Install(context.Background(), &Plan{})
	if err != nil || len(results) != 0 {
		t.Errorf("%v, %v", results, err)
	}
}

func TestInstallIntoABarePrefix(t *testing.T) {
	e := newEnv(t)
	bare := t.TempDir()
	root, err := fsx.Open(bare, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	e.in.Root, e.in.Prefix, e.in.Linker = root, bare, &link.Linker{Root: root, Prefix: bare}
	results, err := e.install(e.item(jq, true, false, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Linked {
		t.Fatalf("result = %+v", results[0])
	}
	for _, d := range []string{"bin", "etc", "include", "lib", "sbin", "share", "opt", "var/homebrew/linked", "Cellar"} {
		if fi, err := os.Stat(filepath.Join(bare, d)); err != nil || !fi.IsDir() {
			t.Errorf("%s is not there: %v", d, err)
		}
	}
}

func TestInstallCreatesFrameworksOnlyOnMacs(t *testing.T) {
	for os_, want := range map[string]bool{"darwin": true, "linux": false} {
		e := newEnv(t)
		e.in.OS = os_
		if _, err := e.install(e.item(jq, true, false, nil)); err != nil {
			t.Fatal(err)
		}
		if got := e.exists("Frameworks"); got != want {
			t.Errorf("%s: Frameworks exists = %v", os_, got)
		}
	}
}

func steps(t *testing.T, raw string) map[string]any {
	t.Helper()
	var v []any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		t.Fatal(err)
	}
	return map[string]any{"post_install_steps": v}
}

func TestInstallRunsPostInstallStepsAfterLinking(t *testing.T) {
	e := newEnv(t)
	extra := steps(t, `[
		{"type":"mkdir_p","path":{"base":"var","path":"lib/{{formula_name}}"}},
		{"type":"symlink","force":true,"source":{"base":"opt_prefix","path":"bin/jq"},"target":{"base":"var","path":"lib/jq/current"}},
		{"type":"write","content":"v={{version}} user={{user}}\n","path":{"base":"pkgetc","path":"state"}}
	]`)
	e.in.User = "tester"
	results, err := e.install(e.item(jq, true, false, extra))
	if err != nil {
		t.Fatal(err)
	}
	if results[0].PostInstallErr != nil {
		t.Fatalf("post_install failed: %v", results[0].PostInstallErr)
	}
	if got := e.readlink("var/lib/jq/current"); got != e.prefix+"/opt/jq/bin/jq" {
		t.Errorf("link = %q: the opt record has to exist when the steps run", got)
	}
	if e.read("etc/jq/state") != "v=1.0 user=tester\n" {
		t.Errorf("state = %q", e.read("etc/jq/state"))
	}
}

func TestInstallRunsPostInstallStepsForTheHostOS(t *testing.T) {
	e := newEnv(t)
	e.in.OS = "linux"
	extra := steps(t, `[
		{"type":"touch","path":{"base":"var","path":"mac"},"guards":[{"condition":"on","value":"macos","id":"1"}]},
		{"type":"touch","path":{"base":"var","path":"lin"},"guards":[{"condition":"on","value":"linux","id":"2"}]}
	]`)
	if _, err := e.install(e.item(jq, true, false, extra)); err != nil {
		t.Fatal(err)
	}
	if e.exists("var/mac") || !e.exists("var/lin") {
		t.Error("the on guards did not follow the installer's OS")
	}
}

func TestInstallPostInstallFailureKeepsTheKegAndGoesOn(t *testing.T) {
	e := newEnv(t)
	bad := e.item(jq, true, false, steps(t, `[{"type":"run","command":{"path":"false"}}]`))
	next := e.item(installtest.Bottle{Name: "next", Version: "1.0", Files: []installtest.File{file("bin/next", "n", 0o755)}}, true, false, nil)
	results, err := e.install(bad, next)
	if err != nil {
		t.Fatalf("a failing post_install must not fail the install: %v", err)
	}
	if len(results) != 2 || results[0].PostInstallErr == nil || results[1].PostInstallErr != nil {
		t.Fatalf("results = %+v", results)
	}
	if !results[0].Linked || !e.exists("Cellar/jq/1.0") || !e.exists("Cellar/next/1.0") {
		t.Error("the keg should stay installed and linked, and the next one should install")
	}
}

func TestInstallSkipsPostInstallWhenThereAreNoSteps(t *testing.T) {
	e := newEnv(t)
	results, err := e.install(e.item(jq, true, false, nil))
	if err != nil || results[0].PostInstallErr != nil {
		t.Errorf("%v, %v", results, err)
	}
}

func TestInstallRestoresBottledEtcAndVar(t *testing.T) {
	e := newEnv(t)
	spec := installtest.Bottle{Name: "cfg", Version: "1.0", Files: []installtest.File{
		file("bin/cfg", "#!/bin/sh\n", 0o755),
		file(".bottle/etc/cfg.yml", "a: 1\n", 0),
		file(".bottle/etc/sub/x.conf", "x\n", 0),
		file(".bottle/var/lib/cfg/", "", 0),
	}}
	if _, err := e.install(e.item(spec, true, false, nil)); err != nil {
		t.Fatal(err)
	}
	if e.read("etc/cfg.yml") != "a: 1\n" || e.read("etc/sub/x.conf") != "x\n" || !e.exists("var/lib/cfg") {
		t.Error("bottled etc and var were not restored into the prefix")
	}
}

func TestInstallKeepsEditedConfigAndAddsDefault(t *testing.T) {
	e := newEnv(t)
	if err := e.root.WriteFile("etc/cfg.yml", []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := installtest.Bottle{Name: "cfg", Version: "1.0", Files: []installtest.File{
		file(".bottle/etc/cfg.yml", "a: 1\n", 0),
	}}
	if _, err := e.install(e.item(spec, true, false, nil)); err != nil {
		t.Fatal(err)
	}
	if e.read("etc/cfg.yml") != "mine\n" || e.read("etc/cfg.yml.default") != "a: 1\n" {
		t.Errorf("etc = %q, default = %q", e.read("etc/cfg.yml"), e.read("etc/cfg.yml.default"))
	}
}

var jqOld = installtest.Bottle{
	Name: "jq", Version: "0.9",
	Files: []installtest.File{
		file("bin/jq", "#!/bin/sh\necho old jq", 0o755),
		file("share/man/man1/jq.1", "old man page", 0),
	},
}

// installOld installs the 0.9 jq, linked or not, and returns an item that upgrades it to 1.0.
func (e *env) installOld(linked bool, onRequest bool) Item {
	e.t.Helper()
	if _, err := e.install(e.item(jqOld, onRequest, !linked, nil)); err != nil {
		e.t.Fatal(err)
	}
	kegs, err := cellar.Prefix(e.prefix).Kegs()
	if err != nil {
		e.t.Fatal(err)
	}
	it := e.item(jq, onRequest, false, nil)
	it.Upgrade = kegs
	it.Linked = &linked
	return it
}

func TestUpgradeUnlinksTheOldKegPoursTheNewOneAndLinksIt(t *testing.T) {
	e := newEnv(t)
	it := e.installOld(true, true)
	e.log.Reset()
	results, err := e.install(it)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Upgraded || results[0].OldVersion != "0.9" || !results[0].Linked {
		t.Fatalf("results = %+v", results)
	}
	if got := e.readlink("bin/jq"); got != "../Cellar/jq/1.0/bin/jq" {
		t.Errorf("bin/jq -> %s", got)
	}
	if got := e.readlink("opt/jq"); got != "../Cellar/jq/1.0" {
		t.Errorf("opt/jq -> %s", got)
	}
	if got := e.readlink("var/homebrew/linked/jq"); got != "../../../Cellar/jq/1.0" {
		t.Errorf("linked record -> %s", got)
	}
	if !e.exists("Cellar/jq/0.9/bin/jq") {
		t.Error("the old keg is left for cleanup to remove")
	}
	if want := "==> Upgrading jq\n  0.9 -> 1.0\n"; !strings.Contains(e.log.String(), want) {
		t.Errorf("log = %q, want it to contain %q", e.log.String(), want)
	}
}

func TestUpgradeKeepsWhatTheOldReceiptSaid(t *testing.T) {
	e := newEnv(t)
	it := e.installOld(true, false)
	if _, err := e.install(it); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(e.read("Cellar/jq/1.0/INSTALL_RECEIPT.json"), `"installed_on_request": true`) {
		t.Error("the upgrade of a dependency should not become on request")
	}
}

func TestUpgradeOfAnUnlinkedKegLeavesTheNewOneUnlinked(t *testing.T) {
	e := newEnv(t)
	it := e.installOld(false, true)
	results, err := e.install(it)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Linked || e.exists("bin/jq") || e.exists("var/homebrew/linked/jq") {
		t.Error("it was not linked before, so it should not be now")
	}
	if got := e.readlink("opt/jq"); got != "../Cellar/jq/1.0" {
		t.Errorf("opt/jq -> %s: the opt record follows the new keg", got)
	}
}

func TestUpgradeThatFailsBeforeAnythingChangesLeavesTheOldKegLinked(t *testing.T) {
	e := newEnv(t)
	it := e.installOld(true, true)
	e.reg.Tamper(it.Bottle) // the download fails its checksum
	if _, err := e.install(it); err == nil {
		t.Fatal("a tampered bottle should fail")
	}
	if got := e.readlink("bin/jq"); got != "../Cellar/jq/0.9/bin/jq" || e.exists("Cellar/jq/1.0") {
		t.Errorf("bin/jq -> %s; the old keg should still be in use", got)
	}
}

func TestUpgradeThatFailsAfterUnlinkingRelinksTheOldKeg(t *testing.T) {
	e := newEnv(t)
	it := e.installOld(true, true)
	if err := os.MkdirAll(filepath.Join(e.prefix, "Cellar/jq/1.0"), 0o755); err != nil { // the new keg's place is taken
		t.Fatal(err)
	}
	if _, err := e.install(it); err == nil {
		t.Fatal("the rename into the Cellar should fail")
	}
	if got := e.readlink("bin/jq"); got != "../Cellar/jq/0.9/bin/jq" {
		t.Errorf("bin/jq -> %s: the old keg should be linked again", got)
	}
	if got := e.readlink("opt/jq"); got != "../Cellar/jq/0.9" {
		t.Errorf("opt/jq -> %s", got)
	}
	if got := e.readlink("var/homebrew/linked/jq"); got != "../../../Cellar/jq/0.9" {
		t.Errorf("linked record -> %s", got)
	}
}

func TestUpgradeThatFailsDoesNotRelinkAKegThatWasNotLinked(t *testing.T) {
	e := newEnv(t)
	it := e.installOld(false, true)
	if err := os.MkdirAll(filepath.Join(e.prefix, "Cellar/jq/1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := e.install(it); err == nil {
		t.Fatal("expected a failure")
	}
	if e.exists("bin/jq") || e.exists("var/homebrew/linked/jq") {
		t.Error("an unlinked keg must stay unlinked")
	}
}

func TestUpgradeKeepsGoingAfterAFormulaFails(t *testing.T) {
	e := newEnv(t)
	first := e.installOld(true, true)
	other := installtest.Bottle{Name: "tool", Version: "0.9", Files: []installtest.File{file("bin/tool", "old", 0o755)}}
	if _, err := e.install(e.item(other, true, false, nil)); err != nil {
		t.Fatal(err)
	}
	kegs, _ := cellar.Prefix(e.prefix).Kegs()
	var toolKegs []cellar.Keg
	for _, k := range kegs {
		if k.Name == "tool" {
			toolKegs = append(toolKegs, k)
		}
	}
	linked := true
	second := e.item(installtest.Bottle{Name: "tool", Version: "1.0", Files: []installtest.File{file("bin/tool", "new", 0o755)}}, true, false, nil)
	second.Upgrade, second.Linked = toolKegs, &linked
	if err := os.MkdirAll(filepath.Join(e.prefix, "Cellar/jq/1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.in.KeepGoing = true
	results, err := e.install(first, second)
	if err != nil {
		t.Fatalf("with KeepGoing a failure is in the results: %v", err)
	}
	if len(results) != 2 || results[0].Err == nil || results[0].Name != "jq" || results[1].Err != nil || !results[1].Upgraded {
		t.Fatalf("results = %+v", results)
	}
	if got := e.readlink("bin/tool"); got != "../Cellar/tool/1.0/bin/tool" {
		t.Errorf("bin/tool -> %s: the second formula should have been upgraded", got)
	}
	if got := e.readlink("bin/jq"); got != "../Cellar/jq/0.9/bin/jq" {
		t.Errorf("bin/jq -> %s: the first stays as it was", got)
	}
}

func TestInstallStillStopsAtTheFirstFailureWithoutKeepGoing(t *testing.T) {
	e := newEnv(t)
	it := e.installOld(true, true)
	if err := os.MkdirAll(filepath.Join(e.prefix, "Cellar/jq/1.0"), 0o755); err != nil {
		t.Fatal(err)
	}
	results, err := e.install(it)
	if err == nil || len(results) != 0 {
		t.Errorf("results = %v, err = %v", results, err)
	}
}

func TestInstallCallsAfterForEachInstalledFormula(t *testing.T) {
	e := newEnv(t)
	var seen []string
	e.in.After = func(it Item, r Result) { seen = append(seen, it.Formula.Name+" "+r.Version) }
	if _, err := e.install(e.item(jq, true, false, nil)); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 1 || seen[0] != "jq 1.0" {
		t.Errorf("seen = %v", seen)
	}
	e.in.After = func(Item, Result) { t.Error("After ran for a failure") }
	e.in.KeepGoing = true
	if _, err := e.install(e.item(jq, true, false, nil)); err != nil { // exists already: fails the rename
		t.Fatal(err)
	}
}

func TestUpgradeNamesTheKegInUseAndLeavesEveryOldKegUnlinked(t *testing.T) {
	e := newEnv(t)
	older := installtest.Bottle{Name: "jq", Version: "0.8", Files: []installtest.File{file("bin/jq", "older", 0o755)}}
	if _, err := e.install(e.item(older, true, true, nil)); err != nil { // keg-only, so not linked
		t.Fatal(err)
	}
	it := e.installOld(true, true) // 0.9 takes over the opt record and the links
	it.Upgrade = nil
	kegs, _ := cellar.Prefix(e.prefix).Kegs()
	it.Upgrade = kegs
	e.log.Reset()
	results, err := e.install(it)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].OldVersion != "0.9" || !strings.Contains(e.log.String(), "  0.9 -> 1.0\n") {
		t.Errorf("old version = %q, log = %q", results[0].OldVersion, e.log.String())
	}
	if got := e.readlink("bin/jq"); got != "../Cellar/jq/1.0/bin/jq" {
		t.Errorf("bin/jq -> %s", got)
	}
}

// shippedSBOM is the SBOM a bottle carries, as the build left it.
const shippedSBOM = `{
  "SPDXID": "SPDXRef-DOCUMENT",
  "creationInfo": {
    "created": "2026-10-01T00:00:00Z",
    "creators": [
      "Tool: https://github.com/Homebrew/brew"
    ]
  },
  "packages": [
    {
      "name": "jq"
    }
  ]
}`

func sbomBottle(supplement string) installtest.Bottle {
	b := jq
	b.Files = append(slices.Clone(jq.Files), file("sbom.spdx.json", shippedSBOM, 0))
	b.SBOMSupplement = supplement
	return b
}

func TestInstallUpdatesTheSBOMOfTheBottle(t *testing.T) {
	e := newEnv(t)
	e.in.SBOMCreator = "Tool: https://github.com/jigsmiths/brw@test"
	if _, err := e.install(e.item(sbomBottle(`{"packages":[{"name":"extra"}]}`), true, false, nil)); err != nil {
		t.Fatal(err)
	}
	got := e.read("Cellar/jq/1.0/sbom.spdx.json")
	for _, want := range []string{`"created": "2026-10-12T12:00:00Z"`, `"Tool: https://github.com/jigsmiths/brw@test"`, `"name": "jq"`, `"name": "extra"`, `"SPDXID": "SPDXRef-DOCUMENT"`} {
		if !strings.Contains(got, want) {
			t.Errorf("the SBOM lacks %s:\n%s", want, got)
		}
	}
	if strings.Contains(got, "2026-10-01") || strings.HasSuffix(got, "\n") {
		t.Errorf("the old time stayed, or a newline was added:\n%s", got)
	}
	// The time is the one in the receipt.
	if r := e.read("Cellar/jq/1.0/INSTALL_RECEIPT.json"); !strings.Contains(r, fmt.Sprintf(`"time": %d`, fixedTime.Unix())) {
		t.Errorf("receipt: %s", r)
	}
	if fi, err := os.Stat(filepath.Join(e.prefix, "Cellar/jq/1.0/sbom.spdx.json")); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v, %v", fi.Mode(), err)
	}
}

func TestInstallTakesTheSupplementOfTheBottleTag(t *testing.T) {
	e := newEnv(t)
	supp := `{"tags":{"arm64_tahoe":{"packages":[{"name":"for-tahoe"}]},"x86_64_linux":{"packages":[{"name":"for-linux"}]}}}`
	if _, err := e.install(e.item(sbomBottle(supp), true, false, nil)); err != nil {
		t.Fatal(err)
	}
	got := e.read("Cellar/jq/1.0/sbom.spdx.json")
	if !strings.Contains(got, "for-tahoe") || strings.Contains(got, "for-linux") {
		t.Errorf("the SBOM:\n%s", got)
	}
}

func TestInstallNamesBrwAsTheCreatorByDefault(t *testing.T) {
	e := newEnv(t)
	if _, err := e.install(e.item(sbomBottle(""), true, false, nil)); err != nil {
		t.Fatal(err)
	}
	if got := e.read("Cellar/jq/1.0/sbom.spdx.json"); !strings.Contains(got, `"Tool: https://github.com/jigsmiths/brw"`) {
		t.Errorf("the SBOM:\n%s", got)
	}
}

func TestInstallLeavesABottleWithoutAnSBOMAndAnUnreadableOneAlone(t *testing.T) {
	e := newEnv(t)
	if _, err := e.install(e.item(jq, true, false, nil)); err != nil {
		t.Fatal(err)
	}
	if e.exists("Cellar/jq/1.0/sbom.spdx.json") {
		t.Error("an SBOM appeared from nowhere")
	}
	broken := jq
	broken.Name = "broken"
	broken.Files = []installtest.File{file("sbom.spdx.json", "{not json", 0), file("bin/broken", "x", 0o755)}
	if _, err := e.install(e.item(broken, true, false, nil)); err != nil {
		t.Fatalf("an unreadable SBOM must not fail the install: %v", err)
	}
	if got := e.read("Cellar/broken/1.0/sbom.spdx.json"); got != "{not json" {
		t.Errorf("the SBOM = %q", got)
	}
}

func TestUpgradeUpdatesTheSBOMToo(t *testing.T) {
	e := newEnv(t)
	old := e.installOld(true, true)
	upgrade := e.item(sbomBottle(`{"packages":[{"name":"extra"}]}`), true, false, nil)
	upgrade.Upgrade, upgrade.Linked = old.Upgrade, old.Linked
	if _, err := e.install(upgrade); err != nil {
		t.Fatal(err)
	}
	if got := e.read("Cellar/jq/1.0/sbom.spdx.json"); !strings.Contains(got, "extra") || !strings.Contains(got, "2026-10-12T12:00:00Z") {
		t.Errorf("the SBOM:\n%s", got)
	}
}

// installCurrent installs jq 1.0, linked or not, and returns an item that reinstalls it.
func (e *env) installCurrent(linked bool) Item {
	e.t.Helper()
	if _, err := e.install(e.item(jq, true, !linked, nil)); err != nil {
		e.t.Fatal(err)
	}
	kegs, err := cellar.Prefix(e.prefix).Kegs()
	if err != nil {
		e.t.Fatal(err)
	}
	it := e.item(jq, true, false, nil)
	it.Upgrade, it.Linked, it.Reinstall = kegs, &linked, true
	return it
}

func TestReinstallPutsTheOldKegAsideAndPoursAgain(t *testing.T) {
	e := newEnv(t)
	it := e.installCurrent(true)
	if err := os.WriteFile(filepath.Join(e.prefix, "Cellar/jq/1.0/bin/jq"), []byte("damaged"), 0o755); err != nil {
		t.Fatal(err)
	}
	e.log.Reset()
	results, err := e.install(it)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].Reinstalled || results[0].OldVersion != "1.0" || !results[0].Linked || results[0].Err != nil {
		t.Fatalf("results = %+v", results)
	}
	if e.read("Cellar/jq/1.0/bin/jq") != "#!/bin/sh\necho jq" {
		t.Error("the keg was not poured afresh")
	}
	if e.exists("Cellar/jq/1.0.reinstall") {
		t.Error("the backup of the old keg should be deleted once the new one is in")
	}
	if got := e.readlink("bin/jq"); got != "../Cellar/jq/1.0/bin/jq" {
		t.Errorf("bin/jq -> %s", got)
	}
	if got := e.readlink("var/homebrew/linked/jq"); got != "../../../Cellar/jq/1.0" {
		t.Errorf("linked record -> %s", got)
	}
	if want := "==> Reinstalling jq\n"; !strings.Contains(e.log.String(), want) || strings.Contains(e.log.String(), "==> Upgrading") {
		t.Errorf("log = %q", e.log.String())
	}
}

func TestReinstallOfAnOutdatedKegReplacesIt(t *testing.T) {
	e := newEnv(t)
	it := e.installOld(true, true) // 0.9, linked, to be replaced by 1.0
	it.Reinstall = true
	if _, err := e.install(it); err != nil {
		t.Fatal(err)
	}
	if e.exists("Cellar/jq/0.9") || e.exists("Cellar/jq/0.9.reinstall") || !e.exists("Cellar/jq/1.0") {
		t.Error("the old version should be gone, unlike after an upgrade")
	}
	if got := e.readlink("bin/jq"); got != "../Cellar/jq/1.0/bin/jq" {
		t.Errorf("bin/jq -> %s", got)
	}
}

func TestReinstallKeepsAnUnlinkedKegUnlinked(t *testing.T) {
	e := newEnv(t)
	it := e.installCurrent(false)
	results, err := e.install(it)
	if err != nil || results[0].Linked || e.exists("bin/jq") || e.exists("var/homebrew/linked/jq") {
		t.Errorf("err = %v, results = %+v", err, results)
	}
	if got := e.readlink("opt/jq"); got != "../Cellar/jq/1.0" {
		t.Errorf("opt/jq -> %s", got)
	}
}

// brokenJq is a jq whose archive is unsafe to unpack, so pouring it fails after
// the old keg was put aside.
func brokenJq() installtest.Bottle {
	return installtest.Bottle{Name: "jq", Version: "1.0", Files: []installtest.File{file("../../escape", "x", 0)}}
}

func TestReinstallThatFailsRestoresTheOldKegAndLinksIt(t *testing.T) {
	e := newEnv(t)
	it := e.installCurrent(true)
	if err := os.WriteFile(filepath.Join(e.prefix, "Cellar/jq/1.0/bin/jq"), []byte("the old one"), 0o755); err != nil {
		t.Fatal(err)
	}
	broken := e.item(brokenJq(), true, false, nil)
	broken.Upgrade, broken.Linked, broken.Reinstall = it.Upgrade, it.Linked, true
	if _, err := e.install(broken); err == nil {
		t.Fatal("the unsafe archive should fail")
	}
	if e.read("Cellar/jq/1.0/bin/jq") != "the old one" || e.exists("Cellar/jq/1.0.reinstall") {
		t.Error("the old keg should be back where it was, and nothing left over")
	}
	for link, want := range map[string]string{"bin/jq": "../Cellar/jq/1.0/bin/jq", "opt/jq": "../Cellar/jq/1.0", "var/homebrew/linked/jq": "../../../Cellar/jq/1.0"} {
		if got := e.readlink(link); got != want {
			t.Errorf("%s -> %s, want %s", link, got, want)
		}
	}
}

func TestReinstallThatFailsLeavesAnUnlinkedKegUnlinked(t *testing.T) {
	e := newEnv(t)
	it := e.installCurrent(false)
	broken := e.item(brokenJq(), true, false, nil)
	broken.Upgrade, broken.Linked, broken.Reinstall = it.Upgrade, it.Linked, true
	if _, err := e.install(broken); err == nil {
		t.Fatal("expected a failure")
	}
	if !e.exists("Cellar/jq/1.0/bin/jq") || e.exists("bin/jq") || e.exists("var/homebrew/linked/jq") {
		t.Error("the keg should be back, and still not linked")
	}
}

func TestReinstallReplacesALeftoverBackup(t *testing.T) {
	e := newEnv(t)
	it := e.installCurrent(true)
	stale := filepath.Join(e.prefix, "Cellar/jq/1.0.reinstall/bin")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "old"), []byte("x"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(stale, 0o555); err != nil {
		t.Fatal(err)
	}
	if _, err := e.install(it); err != nil {
		t.Fatal(err)
	}
	if e.exists("Cellar/jq/1.0.reinstall") {
		t.Error("a leftover backup from an earlier run is replaced and then deleted")
	}
}

func TestReinstallLeavesOtherKegsAlone(t *testing.T) {
	e := newEnv(t)
	older := installtest.Bottle{Name: "jq", Version: "0.8", Files: []installtest.File{file("bin/jq", "older", 0o755)}}
	if _, err := e.install(e.item(older, true, true, nil)); err != nil { // an old keg-only one
		t.Fatal(err)
	}
	it := e.installOld(true, true)
	kegs, _ := cellar.Prefix(e.prefix).Kegs()
	it.Upgrade, it.Reinstall = kegs, true
	if _, err := e.install(it); err != nil {
		t.Fatal(err)
	}
	if !e.exists("Cellar/jq/0.8/bin/jq") || e.exists("Cellar/jq/0.9") || !e.exists("Cellar/jq/1.0") {
		t.Error("only the keg in use is replaced; 0.8 is for cleanup to remove")
	}
}

func TestInstallLocksTheDependenciesInThePlan(t *testing.T) {
	e := newEnv(t)
	held, err := lock.Acquire(e.root, "dep")
	if err != nil {
		t.Fatal(err)
	}
	item := e.item(jq, true, false, nil)
	plan := &Plan{Items: []Item{item}, Locks: []string{"dep", "jq"}}
	if _, err := e.in.Install(context.Background(), plan); !errors.Is(err, lock.ErrInProgress) {
		t.Fatalf("error = %v, want ErrInProgress", err)
	}
	if e.exists("Cellar/jq") {
		t.Error("jq was installed although its dependency was locked")
	}
	if err := held.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.in.Install(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"dep", "jq"} {
		l, err := lock.Acquire(e.root, name)
		if err != nil {
			t.Fatalf("%s was left locked: %v", name, err)
		}
		_ = l.Release()
	}
}
