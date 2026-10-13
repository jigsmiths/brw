package cellar

import (
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// Usage counts the files and bytes of a keg by brew's rules: apparent sizes,
// hard links once, directories and directory symlinks add size but are not
// files, .DS_Store is not a file.
func (k Keg) Usage() (files int, size int64, err error) {
	seen := map[[2]any]bool{} // device and inode; field types differ by platform
	err = filepath.WalkDir(k.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		fi, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			size += fi.Size()
			return nil
		}
		if fi.Mode()&fs.ModeSymlink != 0 {
			if target, err := os.Stat(path); err == nil && target.IsDir() {
				size += fi.Size()
				return nil
			}
		}
		if filepath.Base(path) != ".DS_Store" {
			files++
		}
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			id := [2]any{st.Dev, st.Ino}
			if seen[id] {
				return nil
			}
			seen[id] = true
		}
		size += fi.Size()
		return nil
	})
	return files, size, err
}

// Abv summarises the keg like brew does, for example "1,735 files, 66.3MB".
func (k Keg) Abv() (string, error) {
	files, size, err := k.Usage()
	if err != nil {
		return "", err
	}
	return formatAbv(files, size), nil
}

func formatAbv(files int, size int64) string {
	var b strings.Builder
	if files > 1 {
		b.WriteString(withCommas(files) + " files, ")
	}
	b.WriteString(readableSize(size))
	return b.String()
}

// ReadableSize writes a size like brew, such as "66.3MB".
func ReadableSize(bytes int64) string { return readableSize(bytes) }

func withCommas(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}

// readableSize uses units of 1000 and drops a trailing ".0".
func readableSize(bytes int64) string {
	size, unit := float64(bytes), "B"
	for _, next := range []string{"KB", "MB", "GB"} {
		if math.Round(size*10)/10 < 1000 {
			break
		}
		size /= 1000
		unit = next
	}
	size = math.Round(size*10) / 10
	if size == math.Trunc(size) {
		return strconv.FormatInt(int64(size), 10) + unit
	}
	return strconv.FormatFloat(size, 'f', 1, 64) + unit
}
