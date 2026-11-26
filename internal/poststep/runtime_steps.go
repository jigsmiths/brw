package poststep

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
)

func (r *runner) configureGlibcRuntime() error {
	lib, bin := r.keg("lib"), r.keg("bin")
	if err := r.mkdirAll(filepath.Join(lib, "locale")); err != nil {
		return err
	}
	legacy := r.c.Name != "glibc"
	names := regexp.MustCompile(`^HOMEBREW_LANG$|^LANG$|^LC_`)
	if legacy {
		names = regexp.MustCompile(`^LANG$|^LC_`)
	}
	var locales []string
	for _, e := range os.Environ() {
		key, value, _ := strings.Cut(e, "=")
		if !names.MatchString(key) || value == "C" || (legacy && strings.HasPrefix(value, "C.")) {
			continue
		}
		locales = append(locales, value)
	}
	locales = append(locales, "en_US.UTF-8")
	sort.Strings(locales)
	locales = slices.Compact(locales)
	r.logf("==> Installing locale data for %s", strings.Join(locales, " "))
	for _, locale := range locales {
		lang, charmap, _ := strings.Cut(locale, ".")
		if charmap == "" {
			if err := r.runCommand(filepath.Join(bin, "localedef"), "-i", lang, locale); err != nil {
				return err
			}
			continue
		}
		if charmap == "utf8" {
			charmap = "UTF-8"
		}
		if err := r.runCommand(filepath.Join(bin, "localedef"), "-i", lang, "-f", charmap, locale); err != nil {
			return err
		}
	}
	for _, l := range [][2]string{{"/etc/localtime", filepath.Join(r.keg("etc"), "localtime")}, {"/usr/share/zoneinfo", filepath.Join(r.keg("share"), "zoneinfo")}} {
		_, srcErr := os.Stat(l[0])
		_, dstErr := os.Stat(l[1])
		if srcErr == nil && dstErr != nil {
			if err := r.symlinkAt(l[0], l[1]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *runner) configureGCCRuntime() error {
	if r.c.OS != "linux" {
		return nil
	}
	major, ok := r.versionMajor()
	if !ok {
		return errors.New("GCC runtime configuration requires a version")
	}
	gcc := filepath.Join(r.keg("bin"), "gcc-"+major)
	out, err := r.commandOutput(gcc, "-print-libgcc-file-name")
	if err != nil {
		return err
	}
	libgcc := filepath.Dir(strings.TrimSpace(out))
	glibc := r.anyVersionInstalled("glibc")
	glibcLib := filepath.Join(r.optPrefix("glibc"), "lib")
	crtdir := glibcLib
	if !glibc {
		out, err := r.commandOutput("/usr/bin/cc", "-print-file-name=crti.o")
		if err != nil {
			return err
		}
		crtdir = filepath.Dir(strings.TrimSpace(out))
	}
	crts, _ := filepath.Glob(filepath.Join(crtdir, "*crt?.o"))
	for _, crt := range crts {
		if err := r.removeAny(filepath.Join(libgcc, filepath.Base(crt))); err != nil {
			return err
		}
		if err := r.symlinkAt(crt, filepath.Join(libgcc, filepath.Base(crt))); err != nil {
			return err
		}
	}
	specs := filepath.Join(libgcc, "specs")
	r.logf("==> Creating the GCC specs file: %s", specs)
	for _, f := range []string{specs + ".orig", specs} {
		if err := r.removeAny(f); err != nil {
			return err
		}
	}
	prefix := r.c.Prefix
	headers := []string{filepath.Join(prefix, "include")}
	if glibc {
		headers = append(headers, filepath.Join(r.optPrefix("glibc"), "include"))
	} else {
		target, err := r.commandOutput(gcc, "-print-multiarch")
		if err != nil {
			return err
		}
		headers = append(headers, filepath.Join("/usr/include", strings.TrimSpace(target)), "/usr/include")
	}
	dumped, err := r.commandOutput(gcc, "-dumpspecs")
	if err != nil {
		return err
	}
	if err := r.writeFile(specs+".orig", []byte(dumped), fileMode); err != nil {
		return err
	}
	libdir := filepath.Join(prefix, "lib/gcc/current")
	if r.c.Name != "gcc" {
		libdir = filepath.Join(prefix, "lib/gcc", major)
	}
	linkLibgcc := "+"
	if glibc {
		linkLibgcc = "-nostdlib -L" + libgcc + " -L" + glibcLib
	}
	n, _ := strconv.Atoi(major)
	rpath := n >= 11
	flags := make([]string, len(headers))
	for i, h := range headers {
		flags[i] = "-idirafter " + h
	}
	rpathFlag, rpathSpec := " -rpath "+prefix+"/lib", ""
	if rpath {
		rpathFlag, rpathSpec = "", "*homebrew_rpath:\n-rpath "+prefix+"/lib\n"
	}
	// brew's output is `dumped` (ending in a newline) followed by the heredoc below, which ends in one blank line more.
	content := dumped + "*cpp_unique_options:\n+ -isysroot " + prefix + "/nonexistent " + strings.Join(flags, " ") + "\n\n" +
		"*link_libgcc:\n" + linkLibgcc + " -L" + libdir + " -L" + prefix + "/lib\n\n" +
		"*link:\n+ --dynamic-linker " + prefix + "/lib/ld.so -rpath " + libdir + rpathFlag + "\n\n" +
		rpathSpec + "\n"
	if rpath {
		content = strings.ReplaceAll(content, " %o ", " %o %(homebrew_rpath) ")
	}
	return r.writeFile(specs, []byte(content), fileMode)
}

func (r *runner) configurePHP() error {
	pear := filepath.Join(r.keg("pkgshare"), "pear")
	channels := []string{filepath.Join(pear, ".channels"), filepath.Join(pear, ".channels/.alias")}
	for _, c := range channels {
		if fi, err := os.Stat(c); err == nil && fi.IsDir() {
			if err := r.chmod(c, executableMode); err != nil {
				return err
			}
		}
	}
	var files []string
	for _, f := range []string{".depdblock", ".filemap", ".depdb", ".lock"} {
		if fi, err := os.Stat(filepath.Join(pear, f)); err == nil && fi.Mode().IsRegular() {
			files = append(files, filepath.Join(pear, f))
		}
	}
	for _, c := range channels {
		entries, err := os.ReadDir(c)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if fi, err := os.Stat(filepath.Join(c, e.Name())); err == nil && fi.Mode().IsRegular() {
				files = append(files, filepath.Join(c, e.Name()))
			}
		}
	}
	for _, f := range files {
		if err := r.chmod(f, fileMode); err != nil {
			return err
		}
	}
	pecl := filepath.Join(r.c.Prefix, "lib/php/pecl")
	if err := r.mkdirAll(pecl); err != nil {
		return err
	}
	prefixPecl := filepath.Join(r.keg("prefix"), "pecl")
	if fi, err := os.Lstat(prefixPecl); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if err := r.removeAny(prefixPecl); err != nil {
			return err
		}
	}
	if _, err := os.Stat(prefixPecl); err != nil {
		if err := r.symlinkAt(pecl, prefixPecl); err != nil {
			return err
		}
	}
	extDir, err := r.commandOutput(filepath.Join(r.keg("bin"), "php-config"), "--extension-dir")
	if err != nil {
		return err
	}
	phpBasename := filepath.Base(strings.TrimSpace(extDir))
	if err := r.mkdirAll(filepath.Join(pecl, phpBasename)); err != nil {
		return err
	}
	vmm, ok := r.versionMajorMinor()
	if !ok {
		return errors.New("PHP configuration requires a version")
	}
	pearDir := "pear"
	if r.c.Name != "php" {
		pearDir = "pear@" + vmm
	}
	pearPath := filepath.Join(r.c.Prefix, "share", pearDir)
	if err := r.mkdirAll(pearPath); err != nil {
		return err
	}
	if err := r.copyTreeOver(pear, pearPath); err != nil {
		return err
	}
	opt := r.optPrefix(r.c.Name)
	phpExtDir := filepath.Join(opt, "lib/php", phpBasename)
	etc := filepath.Join(r.keg("etc"), "php", vmm)
	config := []struct{ key, value string }{
		{"php_ini", filepath.Join(etc, "php.ini")},
		{"php_dir", pearPath},
		{"doc_dir", filepath.Join(pearPath, "doc")},
		{"ext_dir", filepath.Join(pecl, phpBasename)},
		{"bin_dir", filepath.Join(opt, "bin")},
		{"data_dir", filepath.Join(pearPath, "data")},
		{"cfg_dir", filepath.Join(pearPath, "cfg")},
		{"www_dir", filepath.Join(pearPath, "htdocs")},
		{"man_dir", filepath.Join(r.c.Prefix, "share/man")},
		{"test_dir", filepath.Join(pearPath, "test")},
		{"php_bin", filepath.Join(opt, "bin/php")},
	}
	pearBin := filepath.Join(r.keg("bin"), "pear")
	for _, c := range config {
		if strings.HasSuffix(c.key, "_dir") && !strings.HasSuffix(c.key, "bin_dir") && !strings.HasSuffix(c.key, "man_dir") {
			if err := r.mkdirAll(c.value); err != nil {
				return err
			}
		}
		if err := r.runCommand(pearBin, "config-set", c.key, c.value, "system"); err != nil {
			return err
		}
	}
	if err := r.runCommand(pearBin, "update-channels"); err != nil {
		return err
	}
	confD := filepath.Join(etc, "conf.d")
	if err := r.mkdirAll(confD); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(phpExtDir, "opcache.so")); err != nil {
		return nil
	}
	ini := filepath.Join(confD, "ext-opcache.ini")
	line := `zend_extension="` + phpExtDir + `/opcache.so"`
	if _, err := os.Stat(ini); err != nil {
		return r.writeFile(ini, []byte("[opcache]\n"+line+"\n"), fileMode)
	}
	return r.inreplace(step{Path: &pathSpec{Path: ini}, Regexp: true, Before: `^\s*zend_extension\s*=.*$`, After: strings.ReplaceAll(line, `\`, `\\`)})
}

func (r *runner) bootstrapCPython() error {
	_ = os.Unsetenv("PYTHONPATH")
	vmm, ok := r.versionMajorMinor()
	if !ok {
		return errors.New("CPython bootstrap requires a version")
	}
	prefix := r.c.Prefix
	sitePackages := filepath.Join(prefix, "lib/python"+vmm, "site-packages")
	var libCellar string
	if r.c.OS == "darwin" {
		libCellar = filepath.Join(r.keg("frameworks"), "Python.framework/Versions", vmm, "lib/python"+vmm)
	} else {
		libCellar = filepath.Join(r.keg("lib"), "python"+vmm)
	}
	siteCellar := filepath.Join(libCellar, "site-packages")
	if err := r.mkdirAll(sitePackages); err != nil {
		return err
	}
	if err := r.removeTree(siteCellar); err != nil {
		return err
	}
	if err := r.installSymlink(filepath.Dir(siteCellar), sitePackages, filepath.Base(sitePackages)); err != nil {
		return err
	}
	for _, pattern := range []string{filepath.Join(sitePackages, "sitecustomize.py[co]")} {
		if err := r.removeGlob(pattern); err != nil {
			return err
		}
	}
	for _, pkg := range []string{"setuptools", "distribute", "pip", "wheel"} {
		for _, pattern := range []string{filepath.Join(sitePackages, pkg+"[-_.][0-9]*"), filepath.Join(sitePackages, pkg)} {
			if err := r.removeGlob(pattern); err != nil {
				return err
			}
		}
	}
	bin := r.keg("bin")
	python := filepath.Join(bin, "python"+vmm)
	if err := r.runCommand(python, "-Im", "ensurepip"); err != nil {
		return err
	}
	bundled := filepath.Join(libCellar, "ensurepip/_bundled")
	var wheels []string
	for _, pattern := range []string{
		filepath.Join(bundled, "setuptools-*-py3-none-any.whl"),
		filepath.Join(bundled, "pip-*-py3-none-any.whl"),
		filepath.Join(r.keg("libexec"), "wheel-*-py3-none-any.whl"),
	} {
		matches, _ := filepath.Glob(pattern)
		if len(matches) != 1 {
			return fmt.Errorf("CPython bootstrap wheel must match exactly one path: %s", pattern)
		}
		wheels = append(wheels, matches[0])
	}
	args := append([]string{"-Im", "pip", "install", "-v", "--no-deps", "--no-index", "--upgrade", "--isolated", "--target=" + sitePackages}, wheels...)
	if err := r.runCommand(python, args...); err != nil {
		return err
	}
	scripts, err := os.ReadDir(filepath.Join(sitePackages, "bin"))
	if err != nil {
		return err
	}
	for _, e := range scripts {
		if err := r.mvInto(filepath.Join(sitePackages, "bin", e.Name()), bin); err != nil {
			return err
		}
	}
	if err := r.removeTree(filepath.Join(sitePackages, "bin")); err != nil {
		return err
	}
	for _, name := range []string{"pip", "pip3"} {
		if err := r.removeTree(filepath.Join(bin, name)); err != nil {
			return err
		}
	}
	if err := r.mvTo(filepath.Join(bin, "wheel"), filepath.Join(bin, "wheel"+vmm)); err != nil {
		return err
	}
	for _, l := range [][2]string{{"pip", "pip" + vmm}, {"pip3", "pip" + vmm}, {"wheel", "wheel" + vmm}, {"wheel3", "wheel" + vmm}} {
		real, err := filepath.EvalSymlinks(filepath.Join(bin, l[1]))
		if err != nil {
			return err
		}
		if err := r.installSymlink(filepath.Join(r.keg("libexec"), "bin"), real, l[0]); err != nil {
			return err
		}
	}
	for _, name := range []string{"wheel" + vmm, "pip" + vmm} {
		if err := r.installSymlink(filepath.Join(prefix, "bin"), filepath.Join(bin, name), name); err != nil {
			return err
		}
	}
	if err := r.makeVenvScriptsWritable(libCellar); err != nil {
		return err
	}
	if vmm != "3.9" {
		return nil
	}
	include := []string{filepath.Join(prefix, "include"), filepath.Join(r.optPrefix("openssl@3"), "include"), filepath.Join(r.optPrefix("sqlite"), "include")}
	lib := []string{filepath.Join(prefix, "lib"), filepath.Join(r.optPrefix("openssl@3"), "lib"), filepath.Join(r.optPrefix("sqlite"), "lib")}
	cfg := filepath.Join(libCellar, "distutils/distutils.cfg")
	if err := r.atomicWrite(cfg, "[install]\nprefix="+prefix+"\n[build_ext]\ninclude_dirs="+strings.Join(include, ":")+"\nlibrary_dirs="+strings.Join(lib, ":")+"\n"); err != nil {
		return err
	}
	compat := filepath.Join(sitePackages, "setuptools/_distutils/command/_framework_compat.py")
	return r.inreplace(step{Path: &pathSpec{Path: compat}, Regexp: true, Before: `^(\s+homebrew_prefix\s+=\s+).*`, After: `\1'` + prefix + `'`})
}

func (r *runner) makeVenvScriptsWritable(libCellar string) error {
	matches, _ := filepath.Glob(filepath.Join(libCellar, "venv/scripts/*"))
	var files []string
	for len(matches) > 0 {
		m := matches[0]
		matches = matches[1:]
		fi, err := os.Stat(m)
		switch {
		case err != nil:
		case fi.IsDir():
			sub, _ := filepath.Glob(filepath.Join(m, "*"))
			matches = append(matches, sub...)
		case fi.Mode().IsRegular():
			files = append(files, m)
		}
	}
	for _, f := range files {
		fi, err := os.Stat(f)
		if err != nil {
			return err
		}
		if err := r.chmod(f, fi.Mode().Perm()|userWrite); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) removeGlob(pattern string) error {
	matches, _ := filepath.Glob(pattern)
	for _, m := range matches {
		if err := r.removeTree(m); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) bootstrapPyPy(abi string) error {
	bin := r.keg("bin")
	pypy := filepath.Join(bin, "pypy"+abi)
	for _, module := range []string{"_sqlite3", "_curses", "syslog", "gdbm", "_tkinter"} {
		if err := r.probeModule(pypy, module); err != nil {
			return err
		}
	}
	prefix, libexec := r.c.Prefix, r.keg("libexec")
	sitePackages := filepath.Join(prefix, "lib/pypy"+abi, "site-packages")
	libexecSite := filepath.Join(libexec, "lib/pypy"+abi, "site-packages")
	scripts := filepath.Join(prefix, "share/pypy"+abi)
	if err := r.mkdirAll(sitePackages); err != nil {
		return err
	}
	if err := r.touchAt(filepath.Join(sitePackages, ".keepme")); err != nil {
		return err
	}
	if err := r.removeTree(libexecSite); err != nil {
		return err
	}
	if err := r.installSymlink(filepath.Dir(libexecSite), sitePackages, filepath.Base(sitePackages)); err != nil {
		return err
	}
	if abi == "3.9" {
		if fi, err := os.Lstat(scripts); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			if err := r.removeAny(scripts); err != nil {
				return err
			}
			entries, _ := os.ReadDir(r.keg("pkgshare"))
			for _, e := range entries {
				if err := r.installSymlink(scripts, filepath.Join(r.keg("pkgshare"), e.Name()), e.Name()); err != nil {
					return err
				}
			}
		}
		if _, err := os.Stat(filepath.Join(libexec, "bin")); err != nil {
			if err := r.installSymlink(libexec, scripts, "bin"); err != nil {
				return err
			}
		}
	}
	if err := r.mkdirAll(scripts); err != nil {
		return err
	}
	if err := r.atomicWrite(filepath.Join(filepath.Dir(libexecSite), "distutils/distutils.cfg"), "[install]\ninstall-scripts="+scripts+"\n"); err != nil {
		return err
	}
	for _, pkg := range []string{"setuptools", "pip"} {
		archive := filepath.Join(libexec, "post-install-resources", pkg+".tar.gz")
		if fi, err := os.Stat(archive); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("PyPy bootstrap archive is missing: %s", archive)
		}
		if err := r.installPyPyPackage(pypy, pkg, archive); err != nil {
			return err
		}
	}
	links := []string{"pip_pypy" + abi}
	if err := r.installSymlink(bin, filepath.Join(scripts, "pip"+abi), "pip_pypy"+abi); err != nil {
		return err
	}
	if r.c.Name == "pypy3" {
		if err := r.installSymlink(bin, "pip_pypy"+abi, "pip_pypy3"); err != nil {
			return err
		}
		links = append(links, "pip_pypy3")
	}
	for _, l := range links {
		if err := r.installSymlink(filepath.Join(prefix, "bin"), filepath.Join(bin, l), l); err != nil {
			return err
		}
	}
	return nil
}

func (r *runner) probeModule(pypy, module string) error {
	c := sudoCommandFor(pypy, "-c", "import "+module)
	_, err := r.sudoCommand(c)
	return err
}

func (r *runner) installPyPyPackage(pypy, pkg, archive string) error {
	tmp, err := os.MkdirTemp(r.tempDir(), "homebrew-pypy-"+pkg) //fsx:allow brew's Dir.mktmpdir in HOMEBREW_TEMP
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }() //fsx:allow brew's Dir.mktmpdir in HOMEBREW_TEMP
	if err := untarGz(archive, tmp); err != nil {
		return err
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return err
	}
	src := tmp
	if len(entries) == 1 && entries[0].IsDir() {
		src = filepath.Join(tmp, entries[0].Name())
	}
	c := sudoCommandFor(pypy, "-s", "setup.py", "--no-user-cfg", "install", "--force", "--verbose")
	c.Dir, c.MustSucceed = src, true
	if r.c.Log != nil {
		c.Stdout, c.Stderr = r.c.Log, r.c.Log
	}
	_, err = r.sudoCommand(c)
	return err
}

func untarGz(archive, dest string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	tr := tar.NewReader(gz)
	root, err := os.OpenRoot(dest) //fsx:allow the archive's scratch directory, outside the prefix
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		name := filepath.Clean(h.Name)
		switch h.Typeflag {
		case tar.TypeDir:
			err = root.MkdirAll(name, dirMode)
		case tar.TypeReg:
			if err = root.MkdirAll(filepath.Dir(name), dirMode); err != nil {
				return err
			}
			var w *os.File
			if w, err = root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(h.Mode).Perm()); err == nil { //fsx:allow the archive's scratch directory, outside the prefix
				_, err = io.Copy(w, tr) //nolint:gosec // the archive is the formula's own bundled resource
				if cerr := w.Close(); err == nil {
					err = cerr
				}
			}
		}
		if err != nil {
			return err
		}
	}
}
