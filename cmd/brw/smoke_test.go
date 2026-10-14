package main

import (
	"bytes"
	"context"
	"debug/elf"
	"io/fs"
	"os"
	osexec "os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"
)

// smoke checks one installed formula without knowing what it is. Problems mean
// the keg is broken. Notes are things worth a look that do not prove a fault,
// like a binary with no --version.
type smoke struct {
	Problems []string
	Notes    []string
}

// smokeFormula checks every keg of a formula in prefix: the receipt is there,
// links in bin lead somewhere, no file keeps a placeholder the bottle should have
// lost, each executable file finds its libraries, and the first program in bin
// runs with --version.
func smokeFormula(prefix, name string) smoke {
	var s smoke
	kegs, _ := filepath.Glob(filepath.Join(prefix, "Cellar", name, "*"))
	if len(kegs) == 0 {
		s.Problems = append(s.Problems, "no keg in the Cellar")
	}
	for _, keg := range kegs {
		if _, err := os.Stat(filepath.Join(keg, "INSTALL_RECEIPT.json")); err != nil {
			s.Problems = append(s.Problems, "no receipt in "+filepath.Base(keg))
		}
		s.walk(prefix, keg)
		s.version(prefix, keg)
	}
	return s
}

var placeholder = regexp.MustCompile(`@@HOMEBREW_[A-Z_]+@@`)

func (s *smoke) walk(prefix, keg string) {
	_ = filepath.WalkDir(keg, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			s.Problems = append(s.Problems, err.Error())
			return nil
		}
		rel, _ := filepath.Rel(keg, p)
		if d.Type()&fs.ModeSymlink != 0 {
			if _, err := os.Stat(p); err != nil && !strings.HasPrefix(rel, "share/doc") {
				s.Problems = append(s.Problems, "dangling link "+rel)
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil {
			s.Problems = append(s.Problems, err.Error())
			return nil
		}
		if m := placeholder.Find(data); m != nil {
			s.Problems = append(s.Problems, rel+" still has "+string(m))
		}
		for _, miss := range missingLibraries(p, data) {
			s.Problems = append(s.Problems, rel+" cannot find "+miss)
		}
		if runtime.GOOS == "darwin" && isMachO(data) {
			// A bad signature gets the program killed on Apple Silicon.
			if out, err := osexec.Command("codesign", "--verify", p).CombinedOutput(); err != nil && !bytes.Contains(out, []byte("not signed at all")) {
				s.Problems = append(s.Problems, rel+": "+firstLine(out))
			}
		}
		return nil
	})
}

// version runs the first program in the keg's bin directory with --version.
func (s *smoke) version(prefix, keg string) {
	entries, _ := os.ReadDir(filepath.Join(keg, "bin"))
	for _, e := range entries {
		p := filepath.Join(keg, "bin", e.Name())
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() || fi.Mode()&0o111 == 0 {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := osexec.CommandContext(ctx, p, "--version")
		cmd.Env = []string{"PATH=" + filepath.Join(prefix, "bin") + ":/usr/bin:/bin", "HOME=" + os.TempDir()}
		if out, err := cmd.CombinedOutput(); err != nil {
			s.Notes = append(s.Notes, e.Name()+" --version: "+err.Error()+": "+firstLine(out))
		}
		return
	}
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(line) > 120 {
		line = line[:120]
	}
	return line
}

// missingLibraries lists the libraries an executable file needs that are not
// there. Files that are not Mach-O or ELF, or that the system tools cannot read,
// have none.
func missingLibraries(path string, data []byte) []string {
	switch {
	case runtime.GOOS == "darwin" && isMachO(data):
		return missingMachO(path)
	case runtime.GOOS == "linux" && bytes.HasPrefix(data, []byte("\x7fELF")):
		return missingELF(path)
	}
	return nil
}

var rpathLine = regexp.MustCompile(`^\s+path (.+) \(offset \d+\)$`)

func missingMachO(path string) []string {
	libs, err := osexec.Command("otool", "-L", path).Output()
	if err != nil {
		return nil
	}
	cmds, _ := osexec.Command("otool", "-l", path).Output()
	var rpaths []string
	for _, l := range strings.Split(string(cmds), "\n") {
		if m := rpathLine.FindStringSubmatch(l); m != nil {
			rpaths = append(rpaths, m[1])
		}
	}
	dir := filepath.Dir(path)
	resolve := func(p string) string {
		p = strings.ReplaceAll(p, "@loader_path", dir)
		return strings.ReplaceAll(p, "@executable_path", dir)
	}
	var missing []string
	for _, l := range strings.Split(string(libs), "\n")[1:] {
		dep, _, _ := strings.Cut(strings.TrimSpace(l), " (")
		switch {
		case dep == "" || filepath.Base(dep) == filepath.Base(path):
			continue
		case strings.HasPrefix(dep, "/usr/lib/") || strings.HasPrefix(dep, "/System/"):
			continue
		case strings.HasPrefix(dep, "@rpath/"):
			found := false
			for _, r := range rpaths {
				if _, err := os.Stat(filepath.Join(resolve(r), strings.TrimPrefix(dep, "@rpath/"))); err == nil {
					found = true
				}
			}
			if !found {
				missing = append(missing, dep)
			}
		default:
			if _, err := os.Stat(resolve(dep)); err != nil && !slices.Contains(missing, dep) {
				missing = append(missing, dep)
			}
		}
	}
	return missing
}

// missingELF asks ldd, which runs the dynamic loader, so a library that is only
// found through an rpath is found too.
func missingELF(path string) []string {
	f, err := elf.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
	if f.Type != elf.ET_EXEC && f.Type != elf.ET_DYN {
		return nil
	}
	out, _ := osexec.Command("ldd", path).CombinedOutput()
	var missing []string
	for _, l := range strings.Split(string(out), "\n") {
		if strings.Contains(l, "not found") {
			missing = append(missing, strings.TrimSpace(strings.Split(l, "=>")[0]))
		}
	}
	return missing
}
