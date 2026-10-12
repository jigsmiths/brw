package relocate

import (
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/jigsmiths/brw/internal/machoedit"
)

var le = binary.LittleEndian

func pad8(n int) int { return (n + 7) &^ 7 }

func dylib(cmd uint32, name string) []byte {
	b := make([]byte, pad8(24+len(name)+1))
	le.PutUint32(b[0:], cmd)
	le.PutUint32(b[4:], uint32(len(b)))
	le.PutUint32(b[8:], 24)
	copy(b[24:], name)
	return b
}

func rpath(path string) []byte {
	b := make([]byte, pad8(12+len(path)+1))
	le.PutUint32(b[0:], 0x8000001c)
	le.PutUint32(b[4:], uint32(len(b)))
	le.PutUint32(b[8:], 12)
	copy(b[12:], path)
	return b
}

// macho builds a thin arm64 file with room for longer names after its commands.
func macho(fileType uint32, cmds ...[]byte) string {
	data := make([]byte, 0x1000)
	le.PutUint32(data[0:], 0xfeedfacf)
	le.PutUint32(data[12:], fileType)
	le.PutUint32(data[16:], uint32(len(cmds)))
	off := 32
	for _, c := range cmds {
		copy(data[off:], c)
		off += len(c)
	}
	le.PutUint32(data[20:], uint32(off-32))
	return string(data)
}

const (
	typeExecute = 2
	typeDylib   = 6
	cmdLoad     = 0xc
	cmdID       = 0xd
)

func TestMachORelocatesNamesLikeBrew(t *testing.T) {
	lib := macho(typeDylib,
		dylib(cmdID, "@@HOMEBREW_PREFIX@@/opt/jq/lib/libjq.1.dylib"),
		dylib(cmdLoad, "@@HOMEBREW_CELLAR@@/jq/1.0/lib/libother.dylib"),
		dylib(cmdLoad, "@@HOMEBREW_PREFIX@@/opt/oniguruma/lib/libonig.5.dylib"),
		dylib(cmdLoad, "/usr/lib/libSystem.B.dylib"),
		dylib(cmdLoad, "@rpath/libkept.dylib"),
		dylib(cmdLoad, "@loader_path/libkept2.dylib"),
		dylib(cmdLoad, "@executable_path/libkept3.dylib"),
		rpath("@@HOMEBREW_PREFIX@@/lib"),
		rpath("@loader_path/../lib"),
	)
	exe := macho(typeExecute, dylib(cmdLoad, "@@HOMEBREW_CELLAR@@/jq/1.0/lib/libjq.1.dylib"))
	untouched := macho(typeExecute, dylib(cmdLoad, "/usr/lib/libSystem.B.dylib"))
	r, dir := stage(t, map[string]string{"lib/libjq.1.dylib": lib, "bin/jq": exe, "bin/plain": untouched})

	modified, err := MachO(r, keg, []string{"lib/libjq.1.dylib", "bin/jq", "bin/plain"}, loc)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"lib/libjq.1.dylib", "bin/jq"}; !reflect.DeepEqual(modified, want) {
		t.Errorf("modified = %v, want %v (a file with nothing to change is left out)", modified, want)
	}

	f, err := machoedit.Parse([]byte(read(t, dir, "lib/libjq.1.dylib")))
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := f.DylibID(); id != "/opt/brw/opt/jq/lib/libjq.1.dylib" {
		t.Errorf("id = %q", id)
	}
	wantLibs := []string{
		"/opt/brw/Cellar/jq/1.0/lib/libother.dylib", "/opt/brw/opt/oniguruma/lib/libonig.5.dylib", "/usr/lib/libSystem.B.dylib",
		"@rpath/libkept.dylib", "@loader_path/libkept2.dylib", "@executable_path/libkept3.dylib",
	}
	if got := f.Dylibs(); !reflect.DeepEqual(got, wantLibs) {
		t.Errorf("dylibs = %v\nwant     %v", got, wantLibs)
	}
	if got := f.Rpaths(); !reflect.DeepEqual(got, []string{"/opt/brw/lib", "@loader_path/../lib"}) {
		t.Errorf("rpaths = %v", got)
	}
	if read(t, dir, "bin/plain") != untouched {
		t.Error("a file with nothing to relocate was rewritten")
	}
}

func TestMachOKeepsModesAndHardLinks(t *testing.T) {
	exe := macho(typeExecute, dylib(cmdLoad, "@@HOMEBREW_PREFIX@@/opt/x/lib/libx.dylib"))
	r, dir := stage(t, map[string]string{"bin/one": exe})
	if err := r.Chmod(keg+"/bin/one", 0o555); err != nil {
		t.Fatal(err)
	}
	if err := r.Link(keg+"/bin/one", keg+"/bin/two"); err != nil {
		t.Fatal(err)
	}
	if _, err := MachO(r, keg, []string{"bin/one"}, loc); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bin/one", "bin/two"} {
		if got := read(t, dir, name); !strings.Contains(got, "/opt/brw/opt/x/lib/libx.dylib") {
			t.Errorf("%s was not relocated", name)
		}
	}
	if fi, _ := os.Stat(filepath.Join(dir, keg, "bin/one")); fi.Mode().Perm() != 0o555 {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
}

func TestMachORefusesWhatItCannotEdit(t *testing.T) {
	good := macho(typeExecute, dylib(cmdLoad, "@@HOMEBREW_PREFIX@@/opt/x/lib/libx.dylib"))
	r, dir := stage(t, map[string]string{
		"text":  "#!/bin/sh\necho not a binary\n",
		"fat":   "\xca\xfe\xba\xbe" + strings.Repeat("\x00", 100),
		"good":  good,
		"short": macho(typeExecute, dylib(cmdLoad, "/usr/lib/libSystem.B.dylib")),
	})
	if _, err := MachO(r, keg, []string{"text"}, loc); !errors.Is(err, machoedit.ErrNotMachO) {
		t.Errorf("text file: %v, want ErrNotMachO", err)
	}
	if _, err := MachO(r, keg, []string{"fat"}, loc); !errors.Is(err, machoedit.ErrUnsupported) {
		t.Errorf("universal file: %v, want ErrUnsupported", err)
	}
	// A failure stops the run, and the files handled before it stay relocated.
	if _, err := MachO(r, keg, []string{"good", "text"}, loc); err == nil {
		t.Error("expected an error")
	}
	if !strings.Contains(read(t, dir, "good"), "/opt/brw/opt/x") {
		t.Error("the earlier file was not relocated")
	}
}

func TestMachOSkipsLikeText(t *testing.T) {
	exe := macho(typeExecute, dylib(cmdLoad, "@@HOMEBREW_PREFIX@@/opt/x/lib/libx.dylib"))
	r, dir := stage(t, map[string]string{"real": exe})
	if err := r.Symlink("real", keg+"/link"); err != nil {
		t.Fatal(err)
	}
	modified, err := MachO(r, keg, []string{"link", "missing", "../escape", "/etc/passwd", ""}, loc)
	if err != nil || len(modified) != 0 {
		t.Errorf("modified = %v, %v", modified, err)
	}
	if read(t, dir, "real") != exe {
		t.Error("a file behind a symlink was relocated")
	}
}

// fakeCodesign records the calls Codesign makes.
type fakeCodesign struct {
	mu      sync.Mutex
	calls   [][]string
	invalid map[string]bool // files whose --verify reports an invalid signature
	failing map[string]int  // files whose signing fails this many times
}

func (f *fakeCodesign) run(args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	if args[0] == "--verify" {
		if f.invalid[args[1]] {
			return []byte(args[1] + ": invalid signature (code or signature have been modified)"), errors.New("exit 1")
		}
		return nil, nil
	}
	for _, file := range args[len(signFlags):] {
		if f.failing[file] > 0 {
			f.failing[file]--
			return []byte("replacing existing signature failed"), errors.New("exit 1")
		}
	}
	return nil, nil
}

func (f *fakeCodesign) signed() []string {
	var files []string
	for _, c := range f.calls {
		if c[0] != "--verify" {
			files = append(files, c[len(signFlags):]...)
		}
	}
	slices.Sort(files)
	return files
}

func TestCodesignAppleSiliconSignsEverythingInBatches(t *testing.T) {
	f := &fakeCodesign{}
	var files []string
	for i := range 40 {
		files = append(files, filepath.Join("/p", string(rune('a'+i%26)), string(rune('a'+i/26)), "f"))
	}
	if err := codesign(files, true, f.run, nil); err != nil {
		t.Fatal(err)
	}
	want := slices.Clone(files)
	slices.Sort(want)
	if got := f.signed(); !reflect.DeepEqual(got, want) {
		t.Errorf("signed %d files, want all %d once", len(got), len(want))
	}
	if len(f.calls) < 2 || len(f.calls) > 40 {
		t.Errorf("%d codesign calls for 40 files; want a few batches", len(f.calls))
	}
	for _, c := range f.calls {
		if !reflect.DeepEqual(c[:len(signFlags)], signFlags) {
			t.Errorf("flags = %v, want %v", c[:len(signFlags)], signFlags)
		}
		if c[0] == "--verify" {
			t.Error("Apple Silicon does not check first, the signature is always broken by the edit")
		}
	}
}

func TestCodesignIntelOnlySignsInvalidSignatures(t *testing.T) {
	f := &fakeCodesign{invalid: map[string]bool{"/p/b": true, "/p/d": true}}
	if err := codesign([]string{"/p/a", "/p/b", "/p/c", "/p/d"}, false, f.run, nil); err != nil {
		t.Fatal(err)
	}
	if got := f.signed(); !reflect.DeepEqual(got, []string{"/p/b", "/p/d"}) {
		t.Errorf("signed %v, want only the invalid ones", got)
	}
}

func TestCodesignRetriesOnAFreshInode(t *testing.T) {
	f := &fakeCodesign{failing: map[string]int{"/p/a": 2, "/p/b": 9}}
	var refreshed []string
	err := codesign([]string{"/p/a", "/p/b", "/p/c"}, true, f.run, func(file string) error {
		refreshed = append(refreshed, file)
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "/p/b") || strings.Contains(err.Error(), "/p/a") {
		t.Errorf("error = %v, want one that names only the file that never signed", err)
	}
	slices.Sort(refreshed)
	if !reflect.DeepEqual(refreshed, []string{"/p/a", "/p/b"}) {
		t.Errorf("refreshed %v, want the two files that failed", refreshed)
	}
	if !slices.Contains(f.signed(), "/p/c") {
		t.Error("a good file in the same batch was not signed")
	}
}

func TestSignStagedMakesFilesWritableAndRestoresThem(t *testing.T) {
	exe := macho(typeExecute, dylib(cmdLoad, "/usr/lib/libSystem.B.dylib"))
	r, dir := stage(t, map[string]string{"bin/one": exe, "bin/two": exe})
	for _, f := range []string{"bin/one", "bin/two"} {
		if err := r.Chmod(keg+"/"+f, 0o555); err != nil {
			t.Fatal(err)
		}
	}
	oneAbs := filepath.Join(dir, keg, "bin/one")
	inode := func(p string) uint64 { fi, _ := os.Stat(p); return fi.Sys().(*syscall.Stat_t).Ino }
	before := inode(oneAbs)

	var writable []bool
	failures := 0 // bin/one fails in the batch and on its own, then signs after the new inode
	run := func(args ...string) ([]byte, error) {
		fails := false
		for _, file := range args[len(signFlags):] {
			fi, err := os.Stat(file)
			if err != nil {
				return nil, err
			}
			writable = append(writable, fi.Mode().Perm()&0o200 != 0)
			if strings.HasSuffix(file, "bin/one") && failures < 2 {
				fails = true
			}
		}
		if fails {
			failures++
			return []byte("flaky"), errors.New("exit 1")
		}
		return nil, nil
	}
	if err := signStaged(r, keg, []string{"bin/one", "bin/two"}, true, run); err != nil {
		t.Fatal(err)
	}
	if len(writable) == 0 || slices.Contains(writable, false) {
		t.Errorf("a read only file was passed to codesign: %v", writable)
	}
	for _, f := range []string{"bin/one", "bin/two"} {
		if fi, _ := os.Stat(filepath.Join(dir, keg, f)); fi.Mode().Perm() != 0o555 {
			t.Errorf("%s mode = %v, want 0555 back", f, fi.Mode().Perm())
		}
	}
	if read(t, dir, "bin/one") != exe {
		t.Error("the copy workaround changed the contents")
	}
	if inode(oneAbs) == before {
		t.Error("the retry did not move the file to a new inode")
	}
}

func TestCodesignNothingToDo(t *testing.T) {
	f := &fakeCodesign{}
	if err := codesign(nil, true, f.run, nil); err != nil || len(f.calls) != 0 {
		t.Errorf("calls = %v, %v", f.calls, err)
	}
}
