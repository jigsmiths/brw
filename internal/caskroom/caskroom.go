// Package caskroom reads the casks installed in a Homebrew prefix.
package caskroom

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Cask is an installed cask.
type Cask struct {
	Token    string
	Version  string // of the latest install
	Caskfile string // the definition stored by that install, .json or .rb
	Tap      string // from the install receipt; empty when it names none or cannot be read
}

func dir(prefix string) string { return filepath.Join(prefix, "Caskroom") }

// Installed lists the installed casks, sorted by token, as brew finds them: a
// directory of the Caskroom, not a symlink, whose latest install under
// .metadata/<version>/<timestamp> stored the cask's definition.
func Installed(prefix string) ([]Cask, error) {
	entries, err := os.ReadDir(dir(prefix))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Cask
	for _, e := range entries {
		if !e.IsDir() { // a symlink is not a directory here
			continue
		}
		c, ok, err := installed(prefix, e.Name())
		if err != nil {
			return nil, fmt.Errorf("cask %s: %w", e.Name(), err)
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func installed(prefix, token string) (Cask, bool, error) {
	meta := filepath.Join(dir(prefix), token, ".metadata")
	versions, err := visible(meta)
	if err != nil {
		return Cask{}, false, err
	}
	// Brew takes the latest timestamp across all versions and does not fall back
	// to an earlier install when that one stored no definition.
	var version, stamp string
	for _, v := range versions {
		stamps, err := visible(filepath.Join(meta, v))
		if err != nil {
			return Cask{}, false, err
		}
		for _, s := range stamps {
			if s > stamp {
				version, stamp = v, s
			}
		}
	}
	if stamp == "" {
		return Cask{}, false, nil
	}
	for _, ext := range []string{".json", ".rb"} {
		file := filepath.Join(meta, version, stamp, "Casks", token+ext)
		if _, err := os.Stat(file); err == nil {
			return Cask{Token: token, Version: version, Caskfile: file, Tap: receiptTap(meta)}, true, nil
		}
	}
	return Cask{}, false, nil
}

// receiptTap is best effort, as brew's is: a cask with no readable receipt
// counts as installed from the API.
func receiptTap(meta string) string {
	b, err := os.ReadFile(filepath.Join(meta, "INSTALL_RECEIPT.json"))
	if err != nil {
		return ""
	}
	var r struct {
		Source struct {
			Tap string `json:"tap"`
		} `json:"source"`
	}
	if json.Unmarshal(b, &r) != nil {
		return ""
	}
	return r.Source.Tap
}

// Pinned returns the version a cask is pinned at, if it is.
func Pinned(prefix, token string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(prefix, "var/homebrew/pinned_casks", token))
	if err != nil {
		return "", false
	}
	return filepath.Base(resolved), true
}

// AppDir is where the cask's apps went, from the configuration stored at
// install: an explicit --appdir, else the default. Like brew, it ignores the
// environment recorded then. brw does not read HOMEBREW_CASK_OPTS.
func AppDir(prefix, token string) string {
	const fallback = "/Applications"
	b, err := os.ReadFile(filepath.Join(dir(prefix), token, ".metadata", "config.json"))
	if err != nil {
		return fallback
	}
	var c struct {
		Explicit, Default struct {
			AppDir string `json:"appdir"`
		}
	}
	if json.Unmarshal(b, &c) != nil {
		return fallback
	}
	for _, d := range []string{c.Explicit.AppDir, c.Default.AppDir} {
		if d != "" {
			return d
		}
	}
	return fallback
}

// visible lists the names in a directory that a shell glob would match. A
// missing directory, or a file in its place, has none.
func visible(path string) ([]string, error) {
	entries, err := os.ReadDir(path)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	return names, nil
}

// Names lists the entries of the Caskroom the way ls does, which is what brew
// list prints, and which of them are symlinks that lead nowhere.
func Names(prefix string) (names, broken []string, err error) {
	entries, err := os.ReadDir(dir(prefix))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		names = append(names, e.Name())
		if e.Type()&fs.ModeSymlink != 0 {
			if _, err := os.Stat(filepath.Join(dir(prefix), e.Name())); err != nil {
				broken = append(broken, e.Name())
			}
		}
	}
	return names, broken, nil
}
