// Package spike creates and removes a throwaway keg to test brew's reaction.
package spike

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
	"github.com/jigsmiths/brw/internal/lock"
)

const receiptTemplate = `{
  "homebrew_version": "6.0.2",
  "used_options": [],
  "unused_options": [],
  "built_as_bottle": true,
  "poured_from_bottle": true,
  "loaded_from_api": true,
  "loaded_from_internal_api": true,
  "installed_on_request": true,
  "changed_files": null,
  "time": %d,
  "source_modified_time": %d,
  "compiler": "clang",
  "aliases": [],
  "runtime_dependencies": [],
  "source": {
    "spec": "stable",
    "versions": {
      "stable": %q,
      "head": null,
      "version_scheme": 0,
      "compatibility_version": null
    },
    "path": "",
    "tap_git_head": null,
    "tap": "homebrew/core"
  },
  "arch": %q,
  "built_on": null
}`

type links struct{ opt, linked, bin string }

func linkTargets(name, version string) links {
	keg := "../Cellar/" + name + "/" + version
	return links{
		opt:    keg,
		linked: "../../" + keg,
		bin:    keg + "/bin/" + name,
	}
}

// CreateKeg installs a fake keg with receipt and links, holding name's lock.
func CreateKeg(root *fsx.Root, name, version string, now time.Time) (err error) {
	l, err := lock.Acquire(root, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, l.Release()) }()

	arch := "arm64"
	if runtime.GOARCH == "amd64" {
		arch = "x86_64"
	}
	receipt, err := cellar.ParseReceipt([]byte(fmt.Sprintf(receiptTemplate, now.Unix(), now.Unix(), version, arch)))
	if err != nil {
		return err
	}
	data, err := receipt.Marshal()
	if err != nil {
		return err
	}

	keg := filepath.Join("Cellar", name, version)
	t := linkTargets(name, version)
	steps := []func() error{
		func() error {
			return root.WriteFile(filepath.Join(keg, "bin", name), []byte("#!/bin/sh\necho "+name+"\n"), 0o755)
		},
		func() error { return root.WriteFile(filepath.Join(keg, "INSTALL_RECEIPT.json"), data, 0o644) },
		func() error { return root.Symlink(t.opt, filepath.Join("opt", name)) },
		func() error { return root.Symlink(t.linked, filepath.Join("var/homebrew/linked", name)) },
		func() error { return root.Symlink(t.bin, filepath.Join("bin", name)) },
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return err
		}
	}
	return nil
}

// RemoveKeg removes a keg made by CreateKeg. It refuses, changing nothing, if
// any of its links points somewhere else.
func RemoveKeg(root *fsx.Root, prefix cellar.Prefix, name, version string) (err error) {
	l, err := lock.Acquire(root, name)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, l.Release()) }()

	t := linkTargets(name, version)
	paths := []struct{ path, want string }{
		{filepath.Join("opt", name), t.opt},
		{filepath.Join("var/homebrew/linked", name), t.linked},
		{filepath.Join("bin", name), t.bin},
	}
	var existing []string
	for _, p := range paths {
		got, err := os.Readlink(filepath.Join(string(prefix), p.path))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if got != p.want {
			return fmt.Errorf("%s points to %s, not %s: not ours, leaving everything", p.path, got, p.want)
		}
		existing = append(existing, p.path)
	}
	for _, p := range existing {
		if err := root.Remove(p); err != nil {
			return err
		}
	}
	if err := root.RemoveAll(filepath.Join("Cellar", name, version)); err != nil {
		return err
	}
	rack := filepath.Join("Cellar", name)
	if entries, err := os.ReadDir(filepath.Join(string(prefix), rack)); err == nil && len(entries) == 0 {
		return root.Remove(rack)
	}
	return nil
}
