// Package pin keeps formulae from being upgraded, with the records brew uses:
// a symlink var/homebrew/pinned/<name> to the pinned keg.
package pin

import (
	"os"
	"path/filepath"

	"github.com/jigsmiths/brw/internal/cellar"
	"github.com/jigsmiths/brw/internal/fsx"
)

const dir = "var/homebrew/pinned"

// Pinned says whether name has a pin record. As in brew, a record whose keg is
// gone still counts.
func Pinned(prefix, name string) bool {
	fi, err := os.Lstat(filepath.Join(prefix, dir, name))
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// Pin records the newest of the kegs, which belong to one formula, as pinned.
func Pin(root *fsx.Root, kegs []cellar.Keg) error {
	k := cellar.Latest(kegs)
	return root.Symlink(filepath.Join("../../../Cellar", k.Name, k.Version), filepath.Join(dir, k.Name))
}

// Unpin removes the record of name, and the directory when that was its last.
func Unpin(root *fsx.Root, name string) error {
	if err := root.Remove(filepath.Join(dir, name)); err != nil {
		return err
	}
	_ = root.Remove(dir) // only succeeds when empty
	return nil
}
