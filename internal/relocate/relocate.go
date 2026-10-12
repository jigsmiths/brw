// Package relocate turns the placeholders in a bottle into paths of this prefix.
package relocate

import (
	"os"
	"path"
	"regexp"
	"strings"
	"syscall"

	"github.com/jigsmiths/brw/internal/fsx"
)

const (
	PrefixPlaceholder     = "@@HOMEBREW_PREFIX@@"
	CellarPlaceholder     = "@@HOMEBREW_CELLAR@@"
	RepositoryPlaceholder = "@@HOMEBREW_REPOSITORY@@"
	LibraryPlaceholder    = "@@HOMEBREW_LIBRARY@@"
	PerlPlaceholder       = "@@HOMEBREW_PERL@@"
	JavaPlaceholder       = "@@HOMEBREW_JAVA@@"
)

// Locations are the paths that replace the placeholders.
type Locations struct {
	Prefix, Cellar, Repository, Library string
	// OpenJDK names the openjdk formula the keg depends on, if any. Without
	// one, the Java placeholder is left alone.
	OpenJDK string
}

// ForPrefix finds brew's repository: the prefix itself, or its Homebrew
// subdirectory (Intel macOS and Linux).
func ForPrefix(prefix string) Locations {
	repo := prefix
	if !isDir(path.Join(prefix, "Library", "Homebrew")) && (prefix == "/usr/local" || isDir(path.Join(prefix, "Homebrew", "Library"))) {
		repo = path.Join(prefix, "Homebrew")
	}
	return Locations{Prefix: prefix, Cellar: path.Join(prefix, "Cellar"), Repository: repo, Library: path.Join(repo, "Library")}
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

var openJDK = regexp.MustCompile(`^openjdk(@\d+(\.\d+)*)?$`)

// OpenJDK returns the first of the formula names that is openjdk or a versioned openjdk.
func OpenJDK(deps []string) string {
	for _, d := range deps {
		if openJDK.MatchString(d) {
			return d
		}
	}
	return ""
}

func (l Locations) replacer() *strings.Replacer {
	pairs := []string{
		RepositoryPlaceholder, l.Repository,
		LibraryPlaceholder, l.Library,
		PrefixPlaceholder, l.Prefix,
		CellarPlaceholder, l.Cellar,
		PerlPlaceholder, l.Prefix + "/opt/perl/bin/perl",
	}
	if l.OpenJDK != "" {
		pairs = append(pairs, JavaPlaceholder, l.Prefix+"/opt/"+l.OpenJDK+"/libexec")
	}
	return strings.NewReplacer(pairs...)
}

// Text replaces the placeholders in the listed files (relative to the keg) and
// returns the ones that changed. As in brew, only regular files reached without
// a symlink are touched, and hard links are rewritten once and reported together.
func Text(root *fsx.Root, keg string, files []string, loc Locations) ([]string, error) {
	type group struct{ names []string }
	var order [][2]any
	groups := map[[2]any]*group{}
	for _, rel := range files {
		if !regularInside(root, keg, rel) {
			continue
		}
		fi, err := root.Lstat(path.Join(keg, rel))
		if err != nil {
			return nil, err
		}
		id := [2]any{rel, nil} // without inode information, every name stands alone
		if st, ok := fi.Sys().(*syscall.Stat_t); ok {
			id = [2]any{st.Dev, st.Ino}
		}
		g := groups[id]
		if g == nil {
			g = &group{}
			groups[id] = g
			order = append(order, id)
		}
		g.names = append(g.names, rel)
	}

	r := loc.replacer()
	var changed []string
	for _, id := range order {
		names := groups[id].names
		file := path.Join(keg, names[0])
		old, err := root.ReadFile(file)
		if err != nil {
			return nil, err
		}
		updated := r.Replace(string(old))
		if updated == string(old) {
			continue
		}
		if err := root.Replace(file, []byte(updated)); err != nil {
			return nil, err
		}
		changed = append(changed, names...)
	}
	return changed, nil
}

// regularInside reports whether keg/rel is a regular file reached without a
// symlink. Lstat does not follow links, so symlinks fail the type checks.
func regularInside(root *fsx.Root, keg, rel string) bool {
	if rel == "" || path.IsAbs(rel) || path.Clean(rel) != rel || rel == ".." || strings.HasPrefix(rel, "../") {
		return false
	}
	cur := keg
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		cur = path.Join(cur, part)
		fi, err := root.Lstat(cur)
		if err != nil {
			return false
		}
		last := i == len(parts)-1
		if last != fi.Mode().IsRegular() || (!last && !fi.IsDir()) {
			return false
		}
	}
	return true
}
