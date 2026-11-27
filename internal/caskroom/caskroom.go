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

type Cask struct {
	Token     string
	Version   string
	Caskfile  string
	Tap       string
	OnRequest bool
}

func dir(prefix string) string { return filepath.Join(prefix, "Caskroom") }

// Installed lists installed casks sorted by token: Caskroom directories, not symlinks, whose latest install stored the cask's definition.
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
		if !e.IsDir() {
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
			c := Cask{Token: token, Version: version, Caskfile: file}
			c.Tap, c.OnRequest = readReceipt(meta)
			return c, true, nil
		}
	}
	return Cask{}, false, nil
}

func readReceipt(meta string) (tap string, onRequest bool) {
	b, err := os.ReadFile(filepath.Join(meta, "INSTALL_RECEIPT.json"))
	if err != nil {
		return "", false
	}
	var r struct {
		Source struct {
			Tap string `json:"tap"`
		} `json:"source"`
		InstalledOnRequest bool `json:"installed_on_request"`
	}
	if json.Unmarshal(b, &r) != nil {
		return "", false
	}
	return r.Source.Tap, r.InstalledOnRequest
}

// DiskUsage sizes each entry of a version's Caskroom directory, following symlinks such as the one to the app.
func DiskUsage(prefix, token, version string) (size int64, ok bool) {
	root := filepath.Join(dir(prefix), token, version)
	entries, err := os.ReadDir(root)
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		size += entryUsage(filepath.Join(root, e.Name()))
	}
	return size, true
}

func entryUsage(path string) int64 {
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&fs.ModeSymlink != 0 {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return 0
		}
		path = resolved
	}
	fi, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	if !fi.IsDir() {
		return fi.Size()
	}
	var size int64
	seen := map[[2]uint64]bool{}
	_ = filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		fi, err := os.Lstat(p)
		if err != nil {
			return nil
		}
		if fi.IsDir() {
			size += fi.Size()
			return nil
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if t, err := os.Stat(p); err == nil && t.IsDir() {
				size += fi.Size()
				return nil
			}
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			id := [2]uint64{uint64(st.Dev), st.Ino} //nolint:unconvert // Dev is int32 on darwin
			if seen[id] {
				return nil
			}
			seen[id] = true
		}
		size += fi.Size()
		return nil
	})
	return size
}

func Pinned(prefix, token string) (string, bool) {
	resolved, err := filepath.EvalSymlinks(filepath.Join(prefix, "var/homebrew/pinned_casks", token))
	if err != nil {
		return "", false
	}
	return filepath.Base(resolved), true
}

type Config struct {
	Default, Env, Explicit map[string]string
}

func ReadConfig(prefix, token string) Config {
	b, err := os.ReadFile(filepath.Join(dir(prefix), token, ".metadata", "config.json"))
	if err != nil {
		return Config{}
	}
	var raw struct {
		Default, Env, Explicit map[string]json.RawMessage
	}
	if json.Unmarshal(b, &raw) != nil {
		return Config{}
	}
	strings := func(m map[string]json.RawMessage) map[string]string {
		out := map[string]string{}
		for k, v := range m {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				out[k] = s
			}
		}
		return out
	}
	return Config{Default: strings(raw.Default), Env: strings(raw.Env), Explicit: strings(raw.Explicit)}
}

func (c Config) Dir(key string) (string, bool) {
	for _, m := range []map[string]string{c.Explicit, c.Env, c.Default} {
		if d, ok := m[key]; ok {
			return d, true
		}
	}
	return "", false
}

func AppDir(prefix, token string) string {
	if d, ok := ReadConfig(prefix, token).Dir("appdir"); ok {
		return d
	}
	return "/Applications"
}

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

// Names lists Caskroom entries as ls does, and which of them are dangling symlinks.
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
