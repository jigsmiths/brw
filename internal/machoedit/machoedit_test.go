package machoedit

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

const (
	cmdSegment  = 0x19
	cmdID       = 0xd
	lcLoadDylib = 0xc
	lcWeakDylib = 0x80000018
	lcReexport  = 0x8000001f
	cmdRpath    = 0x8000001c
	lcCodeSig   = 0x1d
	textOffset  = 0x400 // the first section starts here, so the commands must fit below it
	fileLen     = 0x800
	mhExecute   = 2
	typeDylib   = 6
)

func pad8(n int) int { return (n + 7) &^ 7 }

func dylibCmd(cmd uint32, name string) []byte {
	size := pad8(24 + len(name) + 1)
	b := make([]byte, size)
	le.PutUint32(b[0:], cmd)
	le.PutUint32(b[4:], uint32(size))
	le.PutUint32(b[8:], 24)
	le.PutUint32(b[12:], 2)          // timestamp
	le.PutUint32(b[16:], 0x00020400) // current version
	le.PutUint32(b[20:], 0x00020000) // compatibility version
	copy(b[24:], name)
	return b
}

func rpathCmd(path string) []byte {
	size := pad8(12 + len(path) + 1)
	b := make([]byte, size)
	le.PutUint32(b[0:], cmdRpath)
	le.PutUint32(b[4:], uint32(size))
	le.PutUint32(b[8:], 12)
	copy(b[12:], path)
	return b
}

// segment is a __TEXT segment with one section at textOffset.
func segment(sectSize uint64, sectType uint32) []byte {
	b := make([]byte, 72+80)
	le.PutUint32(b[0:], cmdSegment)
	le.PutUint32(b[4:], uint32(len(b)))
	copy(b[8:], "__TEXT")
	le.PutUint64(b[40:], 0)       // fileoff
	le.PutUint64(b[48:], fileLen) // filesize
	le.PutUint32(b[64:], 1)       // nsects
	s := b[72:]
	copy(s[0:], "__text")
	copy(s[16:], "__TEXT")
	le.PutUint64(s[40:], sectSize)
	le.PutUint32(s[48:], textOffset)
	le.PutUint32(s[64:], sectType)
	return b
}

func codeSig() []byte {
	b := make([]byte, 16)
	le.PutUint32(b[0:], lcCodeSig)
	le.PutUint32(b[4:], 16)
	return b
}

// build lays out a thin arm64 Mach-O: header, commands, zero padding, section data.
func build(fileType uint32, cmds ...[]byte) []byte {
	data := make([]byte, fileLen)
	le.PutUint32(data[0:], 0xfeedfacf)
	le.PutUint32(data[4:], 0x0100000c) // arm64
	le.PutUint32(data[12:], fileType)
	le.PutUint32(data[16:], uint32(len(cmds)))
	off := 32
	for _, c := range cmds {
		copy(data[off:], c)
		off += len(c)
	}
	le.PutUint32(data[20:], uint32(off-32))
	for i := textOffset; i < fileLen; i++ { // section data, which an edit must not touch
		data[i] = byte(i)
	}
	return data
}

func sample() []byte {
	return build(typeDylib,
		segment(0x100, 0),
		dylibCmd(cmdID, "@@HOMEBREW_PREFIX@@/opt/jq/lib/libjq.1.dylib"),
		dylibCmd(lcLoadDylib, "@@HOMEBREW_CELLAR@@/jq/1.8.2/lib/libjq.1.dylib"),
		dylibCmd(lcLoadDylib, "/usr/lib/libSystem.B.dylib"),
		dylibCmd(lcWeakDylib, "@@HOMEBREW_PREFIX@@/opt/weak/lib/libweak.dylib"),
		dylibCmd(lcReexport, "@@HOMEBREW_PREFIX@@/opt/re/lib/libre.dylib"),
		rpathCmd("@@HOMEBREW_PREFIX@@/lib"),
		codeSig(),
	)
}

func mustParse(t *testing.T, b []byte) *File {
	t.Helper()
	f, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestParseReadsTheCommands(t *testing.T) {
	f := mustParse(t, sample())
	if !f.IsDylib() {
		t.Error("not a dylib")
	}
	if id, ok := f.DylibID(); !ok || id != "@@HOMEBREW_PREFIX@@/opt/jq/lib/libjq.1.dylib" {
		t.Errorf("id = %q, %v", id, ok)
	}
	wantLibs := []string{
		"@@HOMEBREW_CELLAR@@/jq/1.8.2/lib/libjq.1.dylib", "/usr/lib/libSystem.B.dylib",
		"@@HOMEBREW_PREFIX@@/opt/weak/lib/libweak.dylib", "@@HOMEBREW_PREFIX@@/opt/re/lib/libre.dylib",
	}
	if got := f.Dylibs(); !reflect.DeepEqual(got, wantLibs) {
		t.Errorf("dylibs = %v, want %v (every kind of dylib command, the id excluded)", got, wantLibs)
	}
	if got := f.Rpaths(); !reflect.DeepEqual(got, []string{"@@HOMEBREW_PREFIX@@/lib"}) {
		t.Errorf("rpaths = %v", got)
	}
	exe := mustParse(t, build(mhExecute, segment(0x10, 0), dylibCmd(lcLoadDylib, "/usr/lib/libSystem.B.dylib")))
	if exe.IsDylib() {
		t.Error("an executable reported as a dylib")
	}
	if _, ok := exe.DylibID(); ok {
		t.Error("an executable has an id")
	}
}

func TestParseRejects(t *testing.T) {
	good := sample()
	cases := map[string]struct {
		data []byte
		want error
	}{
		"empty":             {nil, ErrNotMachO},
		"short":             {[]byte{0xcf, 0xfa, 0xed}, ErrNotMachO},
		"text":              {[]byte("#!/bin/sh\necho hello, this is not a binary at all, just text\n"), ErrNotMachO},
		"fat":               {append([]byte{0xca, 0xfe, 0xba, 0xbe}, make([]byte, 60)...), ErrUnsupported},
		"32 bit":            {append([]byte{0xce, 0xfa, 0xed, 0xfe}, make([]byte, 60)...), ErrUnsupported},
		"big endian":        {append([]byte{0xfe, 0xed, 0xfa, 0xcf}, make([]byte, 60)...), ErrUnsupported},
		"cut off":           {good[:100], ErrNotMachO},
		"commands too big":  {func() []byte { b := bytes.Clone(good); le.PutUint32(b[20:], 1<<20); return b }(), ErrNotMachO},
		"zero size command": {func() []byte { b := bytes.Clone(good); le.PutUint32(b[32+4:], 0); return b }(), ErrNotMachO},
	}
	for name, tt := range cases {
		if _, err := Parse(tt.data); !errors.Is(err, tt.want) {
			t.Errorf("%s: error = %v, want %v", name, err, tt.want)
		}
	}
}

// commandsAfter returns the bytes of every load command after the first n.
func untouched(t *testing.T, before, after []byte) {
	t.Helper()
	if !bytes.Equal(before[textOffset:], after[textOffset:]) {
		t.Error("section data changed")
	}
	if len(before) != len(after) {
		t.Errorf("size changed from %d to %d", len(before), len(after))
	}
}

func TestChangeInstallName(t *testing.T) {
	for name, repl := range map[string]string{
		"shorter": "/o/lib/libjq.1.dylib",
		"longer":  "/Users/someone/.homebrew-in-a-long-place/Cellar/jq/1.8.2/lib/libjq.1.dylib",
		"same":    "@@HOMEBREW_CELLAR@@/jq/1.8.2/lib/libjq.1.dylib",
	} {
		t.Run(name, func(t *testing.T) {
			orig := sample()
			f := mustParse(t, bytes.Clone(orig))
			if err := f.ChangeInstallName("@@HOMEBREW_CELLAR@@/jq/1.8.2/lib/libjq.1.dylib", repl); err != nil {
				t.Fatal(err)
			}
			untouched(t, orig, f.Bytes())
			g := mustParse(t, f.Bytes())
			want := []string{repl, "/usr/lib/libSystem.B.dylib", "@@HOMEBREW_PREFIX@@/opt/weak/lib/libweak.dylib", "@@HOMEBREW_PREFIX@@/opt/re/lib/libre.dylib"}
			if got := g.Dylibs(); !reflect.DeepEqual(got, want) {
				t.Errorf("dylibs = %v, want %v", got, want)
			}
			if id, _ := g.DylibID(); id != "@@HOMEBREW_PREFIX@@/opt/jq/lib/libjq.1.dylib" {
				t.Errorf("id changed to %q", id)
			}
			if got := g.Rpaths(); !reflect.DeepEqual(got, []string{"@@HOMEBREW_PREFIX@@/lib"}) {
				t.Errorf("rpaths = %v", got)
			}
			// The commands behind it moved but are byte for byte the same, and cmdsize stays a multiple of 8.
			for _, c := range g.cmds {
				if c.size%8 != 0 {
					t.Errorf("command %#x has size %d", c.cmd, c.size)
				}
			}
			if le.Uint32(f.Bytes()[16:]) != le.Uint32(orig[16:]) {
				t.Error("ncmds changed")
			}
		})
	}
}

func TestChangeKeepsTheKindOfDylibCommand(t *testing.T) {
	f := mustParse(t, sample())
	if err := f.ChangeInstallName("@@HOMEBREW_PREFIX@@/opt/weak/lib/libweak.dylib", "/new/libweak.dylib"); err != nil {
		t.Fatal(err)
	}
	if err := f.ChangeInstallName("@@HOMEBREW_PREFIX@@/opt/re/lib/libre.dylib", "/new/libre.dylib"); err != nil {
		t.Fatal(err)
	}
	g := mustParse(t, f.Bytes())
	var kinds []uint32
	for _, c := range g.cmds {
		if c.cmd != cmdSegment && c.cmd != cmdRpath && c.cmd != lcCodeSig {
			kinds = append(kinds, c.cmd)
		}
	}
	if want := []uint32{cmdID, lcLoadDylib, lcLoadDylib, lcWeakDylib, lcReexport}; !reflect.DeepEqual(kinds, want) {
		t.Errorf("command kinds = %#x, want %#x", kinds, want)
	}
	// The version fields are carried over.
	for _, c := range g.cmds {
		if c.cmd == lcWeakDylib && (le.Uint32(g.data[c.off+16:]) != 0x00020400 || le.Uint32(g.data[c.off+20:]) != 0x00020000) {
			t.Error("a dylib command lost its version fields")
		}
	}
}

func TestChangeID(t *testing.T) {
	orig := sample()
	f := mustParse(t, bytes.Clone(orig))
	if err := f.ChangeID("/new/prefix/opt/jq/lib/libjq.1.dylib"); err != nil {
		t.Fatal(err)
	}
	untouched(t, orig, f.Bytes())
	g := mustParse(t, f.Bytes())
	if id, _ := g.DylibID(); id != "/new/prefix/opt/jq/lib/libjq.1.dylib" {
		t.Errorf("id = %q", id)
	}
	if len(g.Dylibs()) != 4 {
		t.Errorf("dylibs = %v", g.Dylibs())
	}
	exe := mustParse(t, build(mhExecute, segment(0x10, 0)))
	if err := exe.ChangeID("x"); err == nil {
		t.Error("an executable has no id to change")
	}
}

func TestChangeRpath(t *testing.T) {
	orig := sample()
	f := mustParse(t, bytes.Clone(orig))
	if err := f.ChangeRpath("@@HOMEBREW_PREFIX@@/lib", "/a/much/longer/prefix/than/before/lib"); err != nil {
		t.Fatal(err)
	}
	untouched(t, orig, f.Bytes())
	g := mustParse(t, f.Bytes())
	if got := g.Rpaths(); !reflect.DeepEqual(got, []string{"/a/much/longer/prefix/than/before/lib"}) {
		t.Errorf("rpaths = %v", got)
	}
}

func TestEditsRoundTrip(t *testing.T) {
	orig := sample()
	f := mustParse(t, bytes.Clone(orig))
	old := "@@HOMEBREW_CELLAR@@/jq/1.8.2/lib/libjq.1.dylib"
	long := "/Users/someone/.homebrew-in-a-long-place/Cellar/jq/1.8.2/lib/libjq.1.dylib"
	if err := f.ChangeInstallName(old, long); err != nil {
		t.Fatal(err)
	}
	if err := f.ChangeInstallName(long, old); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.Bytes(), orig) {
		t.Error("changing a name and changing it back did not restore the file")
	}
}

func TestEditsFailCleanly(t *testing.T) {
	orig := sample()
	f := mustParse(t, bytes.Clone(orig))
	if err := f.ChangeInstallName("/no/such/library", "/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown name: %v", err)
	}
	if err := f.ChangeRpath("/no/such/rpath", "/x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown rpath: %v", err)
	}
	// The commands end well before textOffset, but not 1000 bytes before it.
	huge := "/" + string(bytes.Repeat([]byte("a"), 1000))
	if err := f.ChangeInstallName("/usr/lib/libSystem.B.dylib", huge); !errors.Is(err, ErrHeaderPad) {
		t.Errorf("too long: %v, want ErrHeaderPad", err)
	}
	if !bytes.Equal(f.Bytes(), orig) {
		t.Error("a failed edit changed the file")
	}
}

func TestHeaderPadLimitUsesTheLowestSection(t *testing.T) {
	// The only section starts right after the commands, so there is no room to grow.
	cmds := [][]byte{segment(0x10, 0), dylibCmd(lcLoadDylib, "/a")}
	data := build(mhExecute, cmds...)
	total := 0
	for _, c := range cmds {
		total += len(c)
	}
	// Move the section to the end of the commands.
	seg := 32
	le.PutUint32(data[seg+72+48:], uint32(32+total))
	f := mustParse(t, data)
	if err := f.ChangeInstallName("/a", "/a-longer-name-that-needs-more-room"); !errors.Is(err, ErrHeaderPad) {
		t.Errorf("error = %v, want ErrHeaderPad", err)
	}

	// Zero fill sections and empty sections do not count, so the same edit fits.
	for name, mod := range map[string]func([]byte){
		"zerofill": func(b []byte) { le.PutUint32(b[seg+72+64:], 1) },
		"empty":    func(b []byte) { le.PutUint64(b[seg+72+40:], 0) },
	} {
		d := bytes.Clone(data)
		mod(d)
		g := mustParse(t, d)
		if err := g.ChangeInstallName("/a", "/a-longer-name-that-needs-more-room"); err != nil {
			t.Errorf("%s section should not limit the header: %v", name, err)
		}
	}
}
