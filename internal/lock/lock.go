// Package lock takes brew's per-formula locks so brw and brew exclude each other.
package lock

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
)

var (
	ErrInProgress  = errors.New("another process holds the lock")
	ErrInvalidName = errors.New("invalid formula name")
)

type Lock struct {
	f *os.File
}

// Acquire locks name without waiting, at the same path brew uses.
func Acquire(root *fsx.Root, name string) (*Lock, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return nil, fmt.Errorf("%q: %w", name, ErrInvalidName)
	}
	path := "var/homebrew/locks/" + name + ".formula.lock"
	for {
		f, err := root.OpenLock(path)
		if err != nil {
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			_ = f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, fmt.Errorf("%s: %w", name, ErrInProgress)
			}
			return nil, err
		}
		// Retry if the lock file was replaced after opening.
		held, err1 := f.Stat()
		onDisk, err2 := root.Lstat(path)
		if err1 == nil && err2 == nil && os.SameFile(held, onDisk) {
			return &Lock{f: f}, nil
		}
		_ = f.Close()
	}
}

// Release unlocks and keeps the lock file, like brew. Safe to call twice.
func (l *Lock) Release() error {
	if l.f == nil {
		return nil
	}
	err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
