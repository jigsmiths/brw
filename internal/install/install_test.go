package install

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
