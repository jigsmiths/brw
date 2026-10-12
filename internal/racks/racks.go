// Package racks finds the installed formula a name stands for.
package racks

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/jigsmiths/brw/internal/cellar"
)

// NoSuchKegError is a formula that is known but has nothing installed.
type NoSuchKegError struct{ Path string }

func (e *NoSuchKegError) Error() string { return "no such keg: " + e.Path }

// UnavailableError is a name that is neither installed nor a formula.
type UnavailableError struct{ Name string }

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("no available formula with the name %q", e.Name)
}

type Finder struct {
	Prefix string
	Kegs   []cellar.Keg // every installed keg

	// Resolve turns an alias into a formula name and says whether the formula
	// exists. It may be nil when no listing is at hand.
	Resolve func(name string) (canonical string, known bool)
}

// Installed lists the installed kegs of a formula.
func (f Finder) Installed(name string) []cellar.Keg {
	var out []cellar.Keg
	for _, k := range f.Kegs {
		if k.Name == name {
			out = append(out, k)
		}
	}
	return out
}

// Find returns the name of the installed formula that name stands for.
func (f Finder) Find(name string) (string, error) {
	name = strings.TrimPrefix(strings.ToLower(name), "homebrew/core/")
	if len(f.Installed(name)) > 0 {
		return name, nil
	}
	canonical, known := name, false
	if f.Resolve != nil {
		canonical, known = f.Resolve(name)
		if known && len(f.Installed(canonical)) > 0 {
			return canonical, nil
		}
	}
	if known || f.Resolve == nil {
		return "", &NoSuchKegError{Path: filepath.Join(f.Prefix, "Cellar", canonical)}
	}
	return "", &UnavailableError{Name: name}
}
