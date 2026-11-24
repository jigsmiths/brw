package cask

import (
	"context"
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/jigsmiths/brw/internal/api"
	"github.com/jigsmiths/brw/internal/caskroom"
	"github.com/jigsmiths/brw/internal/cellar"
)

type CaskDependency struct {
	FullName         string `json:"full_name"`
	Version          string `json:"version"`
	DeclaredDirectly bool   `json:"declared_directly"`
}

// RuntimeDependencies marshals to {} when empty, with cask before formula as brew writes it.
type RuntimeDependencies struct {
	Cask    []CaskDependency    `json:"cask,omitempty"`
	Formula []cellar.Dependency `json:"formula,omitempty"`
}

// satisfyDependencies runs after the download, as in brew, so a failure leaves nothing staged or replaced.
func (in *Installer) satisfyDependencies(ctx context.Context, c api.Cask) error {
	if in.SatisfyDependencies != nil {
		if err := in.SatisfyDependencies(ctx, c); err != nil {
			return err
		}
	}
	return in.checkDependencies(c)
}

func (in *Installer) checkDependencies(c api.Cask) error {
	for _, name := range c.DependsOn.Formula {
		if !formulaInstalled(in.Where.Prefix, name) {
			return brewError("%s needs the formula %s, which is not installed", c.Token, name)
		}
	}
	if len(c.DependsOn.Cask) == 0 {
		return nil
	}
	installed, err := caskroom.Installed(in.Where.Prefix)
	if err != nil {
		return err
	}
	for _, token := range c.DependsOn.Cask {
		if !slices.ContainsFunc(installed, func(k caskroom.Cask) bool { return k.Token == path.Base(token) }) {
			return brewError("%s needs the cask %s, which is not installed", c.Token, token)
		}
	}
	return nil
}

func formulaInstalled(prefix, name string) bool {
	base := path.Base(name)
	if _, ok := cellar.Prefix(prefix).OptVersion(base); ok {
		return true
	}
	entries, err := os.ReadDir(filepath.Join(prefix, "Cellar", base))
	return err == nil && slices.ContainsFunc(entries, func(e os.DirEntry) bool { return e.IsDir() })
}
