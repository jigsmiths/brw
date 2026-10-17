package relocate

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/machoedit"
)

var invalidSignature = regexp.MustCompile(`(?i)invalid signature`)

// relocatedName maps a name starting with a cellar or prefix placeholder into
// this prefix. Other names (@rpath/x, /usr/lib/x) are left alone.
func (l Locations) relocatedName(name string) (string, bool) {
	switch {
	case strings.HasPrefix(name, CellarPlaceholder):
		return l.Cellar + strings.TrimPrefix(name, CellarPlaceholder), true
	case strings.HasPrefix(name, PrefixPlaceholder):
		return l.Prefix + strings.TrimPrefix(name, PrefixPlaceholder), true
	}
	return "", false
}

// MachO rewrites the id, libraries and rpaths of the listed files and returns
// the ones it changed, which must be signed again (Codesign).
func MachO(root *fsx.Root, keg string, files []string, loc Locations) ([]string, error) {
	var modified []string
	for _, rel := range files {
		if !regularInside(root, keg, rel) {
			continue
		}
		file := path.Join(keg, rel)
		data, err := root.ReadFile(file)
		if err != nil {
			return nil, err
		}
		f, err := machoedit.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		changed, err := relocateFile(f, loc)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", rel, err)
		}
		if !changed {
			continue
		}
		if err := root.Replace(file, f.Bytes()); err != nil {
			return nil, err
		}
		modified = append(modified, rel)
	}
	return modified, nil
}

func relocateFile(f *machoedit.File, loc Locations) (changed bool, err error) {
	if id, ok := f.DylibID(); ok {
		if name, ok := loc.relocatedName(id); ok {
			if err := f.ChangeID(name); err != nil {
				return false, err
			}
			changed = true
		}
	}
	for _, old := range f.Dylibs() {
		if name, ok := loc.relocatedName(old); ok {
			if err := f.ChangeInstallName(old, name); err != nil {
				return false, err
			}
			changed = true
		}
	}
	for _, old := range f.Rpaths() {
		if name, ok := loc.relocatedName(old); ok {
			if err := f.ChangeRpath(old, name); err != nil {
				return false, err
			}
			changed = true
		}
	}
	return changed, nil
}

// signFlags are what brew passes to codesign.
var signFlags = []string{"--sign", "-", "--force", "--preserve-metadata=entitlements,requirements,flags,runtime"}

const signBatch = 16

type runner func(args ...string) ([]byte, error)

func execCodesign(args ...string) ([]byte, error) {
	return exec.Command("codesign", args...).CombinedOutput()
}

// Codesign signs edited files again. Apple Silicon rejects bad signatures, so
// every file is signed; on Intel only invalid ones are, as in brew.
func Codesign(root *fsx.Root, keg string, files []string) error {
	return signStaged(root, keg, files, runtime.GOARCH == "arm64", execCodesign)
}

func signStaged(root *fsx.Root, keg string, rels []string, arm bool, run runner) (err error) {
	byAbs := map[string]string{}
	links := map[string][]string{}
	abs := make([]string, 0, len(rels))
	for _, rel := range rels {
		file := path.Join(keg, rel)
		fi, err := root.Lstat(file)
		if err != nil {
			return err
		}
		if links[file], err = hardLinks(root, keg, file, fi); err != nil {
			return err
		}
		if perm := fi.Mode().Perm(); perm&0o200 == 0 { // codesign has to write the file
			if err := root.Chmod(file, perm|0o200); err != nil {
				return err
			}
			defer func() { err = errors.Join(err, root.Chmod(file, perm)) }()
		}
		p, err := root.Abs(file)
		if err != nil {
			return err
		}
		byAbs[p] = file
		abs = append(abs, p)
	}
	if err := codesign(abs, arm, run, func(p string) error { return newInode(root, byAbs[p]) }); err != nil {
		return err
	}
	return relink(root, links)
}

// hardLinks lists the other names of a file inside the keg. Apple's codesign
// writes a new file in place of the one it signs, which would leave them with the
// old contents and an invalid signature.
func hardLinks(root *fsx.Root, keg, file string, fi fs.FileInfo) ([]string, error) {
	if st, ok := fi.Sys().(*syscall.Stat_t); !ok || uint64(st.Nlink) < 2 { //nolint:unconvert // uint16 on darwin
		return nil, nil
	}
	dir, err := root.Abs(keg)
	if err != nil {
		return nil, err
	}
	var names []string
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		other, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if name := path.Join(keg, filepath.ToSlash(rel)); name != file && os.SameFile(fi, other) {
			names = append(names, name)
		}
		return nil
	})
	return names, err
}

// relink makes the names that were links of a file links of it again.
func relink(root *fsx.Root, links map[string][]string) error {
	for file, names := range links {
		fi, err := root.Lstat(file)
		if err != nil {
			return err
		}
		for _, name := range names {
			if other, err := root.Lstat(name); err == nil && os.SameFile(fi, other) {
				continue
			}
			if err := root.Remove(name); err != nil {
				return err
			}
			if err := root.Link(file, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// newInode copies a file to a new inode under the same name. A known bug in
// Apple's codesign is cured by signing a fresh copy.
func newInode(root *fsx.Root, file string) error {
	fi, err := root.Lstat(file)
	if err != nil {
		return err
	}
	data, err := root.ReadFile(file)
	if err != nil {
		return err
	}
	if err := root.Remove(file); err != nil {
		return err
	}
	return root.WriteFrom(file, bytes.NewReader(data), fi.Mode().Perm()|0o200, fi.ModTime())
}

func signArgs(files ...string) []string { return append(append([]string{}, signFlags...), files...) }

func codesign(files []string, arm bool, run runner, refresh func(file string) error) error {
	if !arm {
		var invalid []string
		for _, f := range files {
			out, err := run("--verify", f)
			if err != nil && invalidSignature.Match(out) {
				invalid = append(invalid, f)
			}
		}
		files = invalid
	}

	var batches [][]string
	for start := 0; start < len(files); start += signBatch {
		batches = append(batches, files[start:min(start+signBatch, len(files))])
	}
	var mu sync.Mutex
	var retry []string
	var wg sync.WaitGroup
	sem := make(chan struct{}, runtime.NumCPU())
	for _, batch := range batches {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if _, err := run(signArgs(batch...)...); err != nil {
				mu.Lock()
				retry = append(retry, batch...)
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	var errs []error
	for _, f := range retry {
		if _, err := run(signArgs(f)...); err == nil {
			continue
		}
		if err := refresh(f); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f, err))
			continue
		}
		if out, err := run(signArgs(f)...); err != nil {
			errs = append(errs, fmt.Errorf("signing %s: %w: %s", f, err, strings.TrimSpace(string(out))))
		}
	}
	return errors.Join(errs...)
}
