// Package cellar reads a Homebrew prefix: kegs, receipts and link records.
package cellar

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Prefix is the root of a Homebrew installation, e.g. /opt/homebrew.
type Prefix string

// Keg is one installed version of a formula.
type Keg struct {
	Name    string
	Version string
	Path    string
	Receipt *Receipt // nil if the keg has no receipt
}

// Kegs lists installed kegs sorted by name, then version. Formula
// directories without any version are skipped.
func (p Prefix) Kegs() ([]Keg, error) {
	cellar := filepath.Join(string(p), "Cellar")
	names, err := os.ReadDir(cellar)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var kegs []Keg
	for _, n := range names {
		if !n.IsDir() {
			continue
		}
		versions, err := os.ReadDir(filepath.Join(cellar, n.Name()))
		if err != nil {
			return nil, err
		}
		for _, v := range versions {
			if !v.IsDir() {
				continue
			}
			k := Keg{Name: n.Name(), Version: v.Name(), Path: filepath.Join(cellar, n.Name(), v.Name())}
			k.Receipt, err = readReceipt(k.Path)
			if err != nil {
				return nil, fmt.Errorf("%s %s: %w", k.Name, k.Version, err)
			}
			kegs = append(kegs, k)
		}
	}
	return kegs, nil
}

func readReceipt(kegPath string) (*Receipt, error) {
	b, err := os.ReadFile(filepath.Join(kegPath, "INSTALL_RECEIPT.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseReceipt(b)
}

// OptVersion returns the version that opt/<name> points to.
func (p Prefix) OptVersion(name string) (string, bool) {
	path, ok := p.resolveKeg(filepath.Join(string(p), "opt", name))
	if !ok {
		return "", false
	}
	return filepath.Base(path), true
}

// Linked reports whether name's linked record points at the given version.
func (p Prefix) Linked(name, version string) bool {
	record := filepath.Join(string(p), "var", "homebrew", "linked", name)
	if fi, err := os.Lstat(record); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	path, ok := p.resolveKeg(record)
	return ok && path == filepath.Join(p.cellarDir(), name, version)
}

func (p Prefix) cellarDir() string {
	return filepath.Join(string(p), "Cellar")
}

// resolveKeg resolves a symlink to an existing directory and returns the
// absolute path it points at, without following symlinks inside the prefix.
func (p Prefix) resolveKeg(link string) (string, bool) {
	target, err := os.Readlink(link)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(link), target)
	}
	target = filepath.Clean(target)
	if fi, err := os.Stat(target); err != nil || !fi.IsDir() {
		return "", false
	}
	return target, true
}
