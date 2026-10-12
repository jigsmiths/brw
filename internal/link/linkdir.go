package link

import (
	"errors"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
)

// ErrAlreadyLinked is returned by Link when the formula has a linked record.
var ErrAlreadyLinked = errors.New("already linked")

// action says what to do with an entry of a keg directory.
type action int

const (
	link   action = iota // a file, or a directory as a whole
	mkpath               // make the directory and go into it
	skipFile
	skipDir
	info
)

// Link links the keg into the prefix as brew does for a formula that is not
// keg-only. If anything fails, what was linked is taken out again.
func (l *Linker) Link(k Keg) (err error) {
	if fi, statErr := l.stat(k.record()); statErr == nil && fi.IsDir() {
		return ErrAlreadyLinked
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, l.Unlink(k))
		}
	}()
	if err := l.Optlink(k); err != nil {
		return err
	}
	for _, d := range []struct {
		dir  string
		rule func(string) action
	}{
		{"etc", func(string) action { return mkpath }},
		{"bin", func(string) action { return skipDir }},
		{"sbin", func(string) action { return skipDir }},
		{"include", includeRule},
		{"share", shareRule},
		{"lib", libRule},
		{"Frameworks", frameworksRule},
	} {
		if err := l.linkDir(k, k.path()+"/"+d.dir, d.rule); err != nil {
			return err
		}
	}
	return l.makeRelativeSymlink(k.record(), k.path())
}

var (
	postgresql = regexp.MustCompile(`^postgresql@\d+`)

	infoFile   = regexp.MustCompile(`info/(?:[^.].*?\.info(?:\.gz)?|dir)$`)
	iconsCache = regexp.MustCompile(`^icons/.*/icon-theme\.cache$`)
	localeDir  = regexp.MustCompile(`(?:locale|man)/(?:[a-z]{2}|C|POSIX)(?:_[A-Z]{2})?(?:\.[a-zA-Z\-0-9]+(?:@.+)?)?`)
	shareDirs  = regexp.MustCompile(`^(?:icons/|zsh|fish|pwsh|lua/|guile/|pypy)`)
	sharePaths = []string{
		"aclocal", "cps", "doc", "info", "java", "locale", "man",
		"man/man1", "man/man2", "man/man3", "man/man4",
		"man/man5", "man/man6", "man/man7", "man/man8",
		"man/cat1", "man/cat2", "man/cat3", "man/cat4",
		"man/cat5", "man/cat6", "man/cat7", "man/cat8",
		"applications", "gnome", "gnome/help", "icons",
		"mime", "mime/packages", "mime-info", "pixmaps",
		"postgresql", "sounds",
	}

	libMade   = []string{"cps", "pkgconfig", "cmake", "dtrace", "ghc", "php"}
	libPrefix = regexp.MustCompile(`^(?:gdk-pixbuf|gio|lua|mecab|node|ocaml|perl5|postgresql@\d+|pypy|python[23]\.\d+|R|ruby)`)

	framework = regexp.MustCompile(`[^/]*\.framework(/Versions)?$`)
)

func includeRule(rel string) action {
	if postgresql.MatchString(rel) {
		return mkpath
	}
	return link
}

func shareRule(rel string) action {
	switch {
	case infoFile.MatchString(rel):
		return info
	case rel == "locale/locale.alias", iconsCache.MatchString(rel):
		return skipFile
	case localeDir.MatchString(rel), shareDirs.MatchString(rel), postgresql.MatchString(rel), slices.Contains(sharePaths, rel):
		return mkpath
	}
	return link
}

func libRule(rel string) action {
	switch {
	case rel == "charset.alias":
		return skipFile
	case slices.Contains(libMade, rel), libPrefix.MatchString(rel):
		return mkpath
	}
	return link
}

func frameworksRule(rel string) action {
	if framework.MatchString(rel) {
		return mkpath
	}
	return link
}

func isPyc(p string) bool {
	ext := path.Ext(p)
	return (ext == ".pyc" || ext == ".pyo") && strings.Contains(p, "/site-packages/")
}

// linkDir links what is below root, a directory of the keg, into the same
// places in the prefix. rule is given each entry's path relative to root.
func (l *Linker) linkDir(k Keg, root string, rule func(rel string) action) error {
	if !l.exists(root) {
		return nil
	}
	var firstErr error
	l.walk(root, false, func(src string, isDir bool) bool {
		if firstErr != nil {
			return false
		}
		dst := strings.TrimPrefix(src, k.path()+"/")
		rel := strings.TrimPrefix(src, root+"/")
		fi, err := l.Root.Lstat(src)
		if err != nil {
			firstErr = err
			return false
		}

		if fi.Mode()&fs.ModeSymlink != 0 || fi.Mode().IsRegular() {
			l.linkFile(&firstErr, src, dst, rel, rule)
			return false
		}
		if !fi.IsDir() {
			return false
		}
		if l.isDir(dst) { // a real directory is already there: go into it
			return true
		}
		if path.Ext(src) == ".app" {
			return false
		}
		switch rule(rel) {
		case skipDir:
			return false
		case mkpath:
			exploded, err := l.resolveAnyConflicts(dst)
			if err == nil && !exploded {
				err = l.Root.MkdirAll(dst, 0o755)
			}
			firstErr = err
			return err == nil
		default:
			exploded, err := l.resolveAnyConflicts(dst)
			if err != nil {
				firstErr = err
				return false
			}
			if exploded {
				return true
			}
			firstErr = l.makeRelativeSymlink(dst, src)
			return false
		}
	})
	return firstErr
}

func (l *Linker) linkFile(errp *error, src, dst, rel string, rule func(string) action) {
	base := path.Base(src)
	if base == ".DS_Store" {
		return
	}
	target := l.resolved(src)
	if target == dst || isOptPathOf(target, dst) || isPyc(src) {
		return
	}
	switch rule(rel) {
	case skipFile:
		return
	case info:
		if base == "dir" { // the historical index
			return
		}
	}
	*errp = l.makeRelativeSymlink(dst, src)
}

// isOptPathOf reports whether target is opt/NAME/rel for any NAME: a link that
// already goes through another keg's opt record.
func isOptPathOf(target, rel string) bool {
	rest, ok := strings.CutPrefix(target, "opt/")
	if !ok {
		return false
	}
	name, tail, ok := strings.Cut(rest, "/")
	return ok && name != "" && tail == rel
}

// resolveAnyConflicts turns dst, a symlink to a directory of another keg, into
// a real directory holding that keg's links, so both kegs can share it, and
// reports whether it did. A dangling link is removed.
func (l *Linker) resolveAnyConflicts(dst string) (bool, error) {
	if !l.isSymlink(dst) {
		return false, nil
	}
	src := l.resolved(dst)
	fi, err := l.Root.Lstat(src)
	if errors.Is(err, fs.ErrNotExist) {
		return false, l.Root.Remove(dst)
	}
	if err != nil || !fi.IsDir() {
		return false, nil
	}
	name, version, ok := kegOf(src)
	if !ok {
		return false, nil
	}
	if err := l.Root.Remove(dst); err != nil {
		return false, err
	}
	other := Keg{Name: name, Version: version}
	if err := l.linkDir(other, src, func(string) action { return mkpath }); err != nil {
		return false, err
	}
	return true, nil
}
