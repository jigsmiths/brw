package install

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/bottle"
	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/link"
	"github.com/jigsmiths/brw/internal/lock"
)

var fixedTime = time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)

// entry is a file of a test bottle, relative to name/version; a name ending in a
// slash is a directory.
type entry struct {
	name, body string
	mode       int64
}

type bottleSpec struct {
	name, version string
	entries       []entry
	changed       []string // the tab's changed_files
	linkage       []string // and linkage_files
	deps          []string // and the formulae in runtime_dependencies
}

// registry serves bottles and their manifests like ghcr.io.
type registry struct {
	t        *testing.T
	server   *httptest.Server
	blobs    map[string][]byte // by digest
	tabs     map[string]string // manifest tag path to tab JSON
	digests  map[string]string // manifest tag path to digest
	tamper   map[string]bool
	manifest atomic.Int32
}

func newRegistry(t *testing.T) *registry {
	t.Helper()
	r := &registry{t: t, blobs: map[string][]byte{}, tabs: map[string]string{}, digests: map[string]string{}, tamper: map[string]bool{}}
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		digest := strings.TrimPrefix(req.URL.Path, "/blob/")
		body := r.blobs[digest]
		if r.tamper[digest] {
			body = append([]byte("x"), body...)
		}
		_, _ = w.Write(body)
	}))
	r.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := strings.TrimPrefix(req.URL.Path, "/v2/homebrew/core/")
		switch {
		case strings.Contains(path, "/manifests/"):
			r.manifest.Add(1)
			digest := r.digests[path]
			_ = json.NewEncoder(w).Encode(map[string]any{"manifests": []map[string]any{
				{"annotations": map[string]string{"sh.brew.bottle.digest": digest, "sh.brew.tab": r.tabs[path]}},
			}})
		case strings.Contains(path, "/blobs/sha256:"):
			_, digest, _ := strings.Cut(path, "sha256:")
			http.Redirect(w, req, cdn.URL+"/blob/"+digest, http.StatusTemporaryRedirect)
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(func() { r.server.Close(); cdn.Close() })
	return r
}

// add stores a bottle and returns its file as the API would describe it.
func (r *registry) add(spec bottleSpec, cellar string) api.BottleFile {
	r.t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	keg := spec.name + "/" + spec.version
	write := func(name string, typ byte, body string, mode int64) {
		h := &tar.Header{Name: name, Typeflag: typ, Mode: mode, Size: int64(len(body)), ModTime: fixedTime}
		if err := tw.WriteHeader(h); err != nil {
			r.t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			r.t.Fatal(err)
		}
	}
	write(keg, tar.TypeDir, "", 0o755)
	for _, e := range spec.entries {
		if strings.HasSuffix(e.name, "/") {
			write(keg+"/"+e.name, tar.TypeDir, "", 0o755)
		} else {
			write(keg+"/"+e.name, tar.TypeReg, e.body, cmpMode(e.mode))
		}
	}
	if err := tw.Close(); err != nil {
		r.t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		r.t.Fatal(err)
	}
	sum := sha256.Sum256(buf.Bytes())
	digest := hex.EncodeToString(sum[:])
	r.blobs[digest] = buf.Bytes()

	var runtime []map[string]any
	for _, d := range spec.deps {
		runtime = append(runtime, map[string]any{"full_name": d, "version": "1.0", "revision": 0, "bottle_rebuild": 0, "pkg_version": "1.0", "declared_directly": true})
	}
	tab, err := json.Marshal(map[string]any{
		"homebrew_version": "6.0.22", "changed_files": spec.changed, "linkage_files": spec.linkage, "binary_relocation_files": []string{},
		"source_modified_time": 1, "compiler": "clang", "runtime_dependencies": runtime, "arch": "arm64",
		"built_on": map[string]any{"os": "Macintosh"},
	})
	if err != nil {
		r.t.Fatal(err)
	}
	r.tabs[spec.name+"/manifests/"+spec.version] = string(tab)
	r.digests[spec.name+"/manifests/"+spec.version] = digest
	return api.BottleFile{Cellar: cellar, SHA256: digest, URL: r.server.URL + "/v2/homebrew/core/" + spec.name + "/blobs/sha256:" + digest}
}

func cmpMode(m int64) int64 {
	if m == 0 {
		return 0o644
	}
	return m
}

type env struct {
	t      *testing.T
	prefix string
	root   *fsx.Root
	reg    *registry
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
	e := &env{t: t, prefix: prefix, root: root, reg: newRegistry(t)}
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

func (e *env) item(spec bottleSpec, onRequest, kegOnly bool, extra map[string]any) Item {
	e.t.Helper()
	cellarKind := ":any"
	return Item{
		Formula: formulaJSON(e.t, spec.name, extra), Tag: "arm64_tahoe", Bottle: e.reg.add(spec, cellarKind),
		PkgVersion: spec.version, OnRequest: onRequest, KegOnly: kegOnly,
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

var jq = bottleSpec{
	name: "jq", version: "1.0",
	entries: []entry{
		{"bin/jq", "#!/bin/sh\necho jq", 0o755},
		{"lib/pkgconfig/jq.pc", "prefix=@@HOMEBREW_CELLAR@@/jq/1.0\nlibdir=@@HOMEBREW_PREFIX@@/lib\n", 0},
		{"share/man/man1/jq.1", "man page", 0},
	},
	changed: []string{"lib/pkgconfig/jq.pc"},
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
	dep := bottleSpec{name: "libx", version: "2.0", entries: []entry{{"lib/libx.a", "x", 0}}}
	app := bottleSpec{name: "app", version: "1.0", entries: []entry{{"bin/app", "app", 0o755}}, deps: []string{"libx"}}
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
	spec := bottleSpec{name: "tool", version: "1.0", entries: []entry{{"bin/tool", machO("@@HOMEBREW_PREFIX@@/opt/dep/lib/libdep.dylib"), 0o755}}, linkage: []string{"bin/tool"}}
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
	spec := bottleSpec{name: "tool", version: "1.0", entries: []entry{{"bin/tool", machO("@@HOMEBREW_PREFIX@@/x"), 0o755}, {"share/t.txt", "@@HOMEBREW_PREFIX@@/etc", 0}}, changed: []string{"share/t.txt"}, linkage: []string{"bin/tool"}}
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
	e.reg.tamper[good.Bottle.SHA256] = true // the registry serves other bytes than the checksum says
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
	bad := bottleSpec{name: "evil", version: "1.0", entries: []entry{{"../../escape", "x", 0}}}
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
	first := e.item(bottleSpec{name: "first", version: "1.0", entries: []entry{{"bin/first", "1", 0o755}}}, true, false, nil)
	second := e.item(bottleSpec{name: "second", version: "1.0", entries: []entry{{"../../x", "2", 0}}}, true, false, nil)
	third := e.item(bottleSpec{name: "third", version: "1.0", entries: []entry{{"bin/third", "3", 0o755}}}, true, false, nil)
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
	before := e.reg.manifest.Load()
	if _, err := e.install(item); !errors.Is(err, lock.ErrInProgress) {
		t.Errorf("error = %v, want ErrInProgress", err)
	}
	if e.exists("Cellar/jq") || e.reg.manifest.Load() != before {
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
	a := e.item(bottleSpec{name: "a", version: "1.0", entries: []entry{{"bin/a", "a", 0o755}}}, true, false, nil)
	b := e.item(bottleSpec{name: "b", version: "1.0", entries: []entry{{"bin/b", "b", 0o755}}}, true, false, nil)
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
