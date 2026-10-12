// Package machoedit rewrites the library names in a Mach-O file the way brew
// does when it pours a bottle: a load command is replaced in place, the ones
// behind it shift, and the file keeps its size.
package machoedit

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

var (
	ErrNotMachO    = errors.New("not a Mach-O file")
	ErrUnsupported = errors.New("unsupported Mach-O layout")
	ErrHeaderPad   = errors.New("not enough room in the Mach-O header")
	ErrNotFound    = errors.New("not found in the Mach-O file")
)

const (
	headerSize = 32 // mach_header_64

	magic64 = 0xfeedfacf

	mhDylib = 6

	lcSegment64 = 0x19
	lcIDDylib   = 0xd
	lcRpath     = 0x8000001c

	dylibHeader = 24 // dylib_command up to the name
	rpathHeader = 12 // rpath_command up to the path

	sZerofill            = 0x1
	sThreadLocalZerofill = 0x12
)

// The load commands that name a library to load.
var loadDylib = []uint32{0xc, 0x80000018, 0x8000001f, 0x20, 0x80000023}

var le = binary.LittleEndian

type command struct {
	off, size int
	cmd       uint32
}

// File is a thin, 64 bit, little endian Mach-O file held in memory.
type File struct {
	data []byte
	cmds []command
}

// Parse reads the header and load commands. Universal files, 32 bit files and
// big endian files are refused instead of being edited wrongly.
func Parse(data []byte) (*File, error) {
	if len(data) < 4 {
		return nil, ErrNotMachO
	}
	switch magic := le.Uint32(data); magic {
	case magic64:
	case 0xbebafeca, 0xcafebabe, 0xbfbafeca, 0xcafebabf: // fat, either byte order
		return nil, fmt.Errorf("%w: universal binary", ErrUnsupported)
	case 0xfeedface, 0xcefaedfe, 0xcffaedfe:
		return nil, fmt.Errorf("%w: 32 bit or big endian", ErrUnsupported)
	default:
		return nil, ErrNotMachO
	}
	if len(data) < headerSize {
		return nil, ErrNotMachO
	}
	f := &File{data: data}
	return f, f.populate()
}

func (f *File) populate() error {
	ncmds, sizeofcmds := int(le.Uint32(f.data[16:])), int(le.Uint32(f.data[20:]))
	if headerSize+sizeofcmds > len(f.data) {
		return fmt.Errorf("%w: load commands run past the end of the file", ErrNotMachO)
	}
	f.cmds = f.cmds[:0]
	off := headerSize
	for range ncmds {
		if off+8 > headerSize+sizeofcmds {
			return fmt.Errorf("%w: truncated load commands", ErrNotMachO)
		}
		size := int(le.Uint32(f.data[off+4:]))
		if size < 8 || off+size > headerSize+sizeofcmds {
			return fmt.Errorf("%w: bad load command size %d", ErrNotMachO, size)
		}
		f.cmds = append(f.cmds, command{off: off, size: size, cmd: le.Uint32(f.data[off:])})
		off += size
	}
	return nil
}

// Bytes returns the file contents, which edits change in place.
func (f *File) Bytes() []byte { return f.data }

func (f *File) IsDylib() bool { return le.Uint32(f.data[12:]) == mhDylib }

// str reads the string a load command points at with its offset field.
func (f *File) str(c command, fieldOff int) string {
	start := c.off + int(le.Uint32(f.data[c.off+fieldOff:]))
	if start >= c.off+c.size {
		return ""
	}
	end := c.off + c.size
	s := f.data[start:end]
	if i := bytes.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return string(s)
}

// DylibID is the install name of a dylib, if the file has one.
func (f *File) DylibID() (string, bool) {
	if !f.IsDylib() {
		return "", false
	}
	for _, c := range f.cmds {
		if c.cmd == lcIDDylib {
			return f.str(c, 8), true
		}
	}
	return "", false
}

// Dylibs lists the libraries the file loads, without repeats, in order.
func (f *File) Dylibs() []string {
	var names []string
	for _, c := range f.cmds {
		if slices.Contains(loadDylib, c.cmd) {
			if name := f.str(c, 8); !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func (f *File) Rpaths() []string {
	var paths []string
	for _, c := range f.cmds {
		if c.cmd == lcRpath {
			paths = append(paths, f.str(c, 8))
		}
	}
	return paths
}

// ChangeID replaces the install name of a dylib.
func (f *File) ChangeID(name string) error {
	if !f.IsDylib() {
		return fmt.Errorf("%w: not a dylib", ErrNotFound)
	}
	for _, c := range f.cmds {
		if c.cmd == lcIDDylib {
			return f.replace(c, f.dylibCommand(c, name))
		}
	}
	return fmt.Errorf("%w: no dylib id", ErrNotFound)
}

// ChangeInstallName replaces the first load command for the library old.
func (f *File) ChangeInstallName(old, name string) error {
	for _, c := range f.cmds {
		if slices.Contains(loadDylib, c.cmd) && f.str(c, 8) == old {
			return f.replace(c, f.dylibCommand(c, name))
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, old)
}

// ChangeRpath replaces the first rpath equal to old.
func (f *File) ChangeRpath(old, path string) error {
	for _, c := range f.cmds {
		if c.cmd == lcRpath && f.str(c, 8) == old {
			return f.replace(c, command2(lcRpath, rpathHeader, path, nil))
		}
	}
	return fmt.Errorf("%w: %s", ErrNotFound, old)
}

// dylibCommand builds a command like c, of the same kind and with the same
// timestamp and versions, that names a different library.
func (f *File) dylibCommand(c command, name string) []byte {
	return command2(c.cmd, dylibHeader, name, f.data[c.off+12:c.off+24])
}

// command2 serializes a command: cmd, size, string offset, the fixed fields,
// then the string and zeros up to a multiple of 8.
func command2(cmd uint32, header int, s string, fixed []byte) []byte {
	size := (header + len(s) + 1 + 7) &^ 7
	b := make([]byte, size)
	le.PutUint32(b[0:], cmd)
	le.PutUint32(b[4:], uint32(size))
	le.PutUint32(b[8:], uint32(header))
	copy(b[12:], fixed)
	copy(b[header:], s)
	return b
}

// replace swaps a command for new bytes, shifting the commands behind it and
// padding or consuming the zeros after the last command. The file keeps its size.
func (f *File) replace(c command, repl []byte) error {
	sizeofcmds := int(le.Uint32(f.data[20:]))
	newSize := sizeofcmds - c.size + len(repl)
	if headerSize+newSize > f.lowFileoff() {
		return fmt.Errorf("%w: need %d bytes of load commands", ErrHeaderPad, newSize)
	}
	end := headerSize + sizeofcmds
	tail := bytes.Clone(f.data[c.off+c.size : end])
	copy(f.data[c.off:], repl)
	copy(f.data[c.off+len(repl):], tail)
	clear(f.data[headerSize+newSize : max(end, headerSize+newSize)]) // what shrinking left behind
	le.PutUint32(f.data[20:], uint32(newSize))
	return f.populate()
}

// lowFileoff is the lowest file offset of any section or of any segment that
// has none, which is where the load commands must stop.
func (f *File) lowFileoff() int {
	low := len(f.data)
	for _, c := range f.cmds {
		if c.cmd != lcSegment64 || c.size < 72 {
			continue
		}
		fileoff, filesize, nsects := le.Uint64(f.data[c.off+40:]), le.Uint64(f.data[c.off+48:]), int(le.Uint32(f.data[c.off+64:]))
		if nsects == 0 && fileoff > 0 && filesize > 0 && fileoff < uint64(low) {
			low = int(fileoff)
		}
		for i := range nsects {
			s := c.off + 72 + i*80
			if s+80 > c.off+c.size {
				break
			}
			size, offset, typ := le.Uint64(f.data[s+40:]), le.Uint32(f.data[s+48:]), le.Uint32(f.data[s+64:])&0xff
			if size == 0 || typ == sZerofill || typ == sThreadLocalZerofill {
				continue
			}
			if int(offset) < low {
				low = int(offset)
			}
		}
	}
	return low
}
