package poststep

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/jigsmiths/brw/internal/sudo"
)

// answers makes the stub runner reply to a command by its executable's base name and first argument.
func (f *sudoFixture) answers(replies map[string]string) {
	f.ctx.Sudo = func(_ context.Context, c sudo.Command) (sudo.Result, error) {
		f.stub.cmds = append(f.stub.cmds, c)
		key := filepath.Base(c.Exe)
		if len(c.Args) > 0 {
			key += " " + c.Args[0]
		}
		return sudo.Result{Stdout: replies[key]}, nil
	}
}

func gzTar(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	_, _ = tw.Write([]byte(body))
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func (f *sudoFixture) keg(rel string) string {
	return filepath.Join(f.prefix, "Cellar/pkg/1.2.3-1", rel)
}

func (f *sudoFixture) ran(exe string) []sudo.Command {
	var out []sudo.Command
	for _, c := range f.stub.cmds {
		if filepath.Base(c.Exe) == exe {
			out = append(out, c)
		}
	}
	return out
}

func TestConfigureGCCRuntimeWritesTheSpecsOnLinux(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.OS, f.ctx.Version = "linux", "12.4.0"
	libgcc := filepath.Join(f.dir, "lib/gcc/libgcc.a")
	crt := filepath.Join(f.dir, "sys")
	f.mkdir("Cellar/pkg/1.2.3-1/bin")
	if err := os.MkdirAll(crt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(crt, "Scrt1.o"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(libgcc), 0o755); err != nil {
		t.Fatal(err)
	}
	f.answers(map[string]string{
		"gcc-12 -print-libgcc-file-name": libgcc + "\n",
		"cc -print-file-name=crti.o":     filepath.Join(crt, "crti.o") + "\n",
		"gcc-12 -print-multiarch":        "x86_64-linux-gnu\n",
		"gcc-12 -dumpspecs":              "*link:\n%l %o x\n",
	})
	f.mustRun(`[{"type":"configure_gcc_runtime"}]`)
	specs := f.slurp(filepath.Join(filepath.Dir(libgcc), "specs"))
	for _, want := range []string{
		"-idirafter " + f.prefix + "/include -idirafter /usr/include/x86_64-linux-gnu -idirafter /usr/include",
		"*link_libgcc:\n+ -L" + f.prefix + "/lib/gcc/12 -L" + f.prefix + "/lib",
		"*homebrew_rpath:\n-rpath " + f.prefix + "/lib\n",
		"%l %o %(homebrew_rpath) x",
	} {
		if !strings.Contains(specs, want) {
			t.Errorf("specs lack %q:\n%s", want, specs)
		}
	}
	if f.slurp(filepath.Join(filepath.Dir(libgcc), "specs.orig")) != "*link:\n%l %o x\n" {
		t.Error("the original specs were not kept")
	}
	if got, _ := os.Readlink(filepath.Join(filepath.Dir(libgcc), "Scrt1.o")); got != filepath.Join(crt, "Scrt1.o") {
		t.Errorf("crt link = %q", got)
	}
	if !strings.Contains(f.log.String(), "==> Creating the GCC specs file: "+filepath.Join(filepath.Dir(libgcc), "specs")) {
		t.Errorf("log = %q", f.log.String())
	}
}

func TestConfigureGCCRuntimeNeedsAVersion(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.OS, f.ctx.Version = "linux", ""
	if err := f.run(`[{"type":"configure_gcc_runtime"}]`); err == nil || !strings.Contains(err.Error(), "GCC runtime configuration requires a version") {
		t.Errorf("err = %v", err)
	}
}

func TestConfigurePHPSetsUpPearAndOpcache(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.Name, f.ctx.Version = "php@8.4", "8.4.1"
	f.write("Cellar/pkg/1.2.3-1/share/php@8.4/pear/.channels/pear.php.net.reg", "reg", 0o600)
	f.write("Cellar/pkg/1.2.3-1/share/php@8.4/pear/.lock", "", 0o600)
	f.write("opt/php@8.4/lib/php/20240924/opcache.so", "so", 0o644)
	f.answers(map[string]string{"php-config --extension-dir": "/x/lib/php/20240924\n"})
	f.mustRun(`[{"type":"configure_php"}]`)
	pear := filepath.Join(f.prefix, "share/pear@8.4")
	if f.slurp(filepath.Join(pear, ".channels/pear.php.net.reg")) != "reg" {
		t.Error("pear was not copied")
	}
	if fi, _ := os.Stat(filepath.Join(f.keg("share/php@8.4/pear/.lock"))); fi.Mode().Perm() != 0o644 {
		t.Errorf("lock mode = %v", fi.Mode())
	}
	if got, _ := os.Readlink(f.keg("pecl")); got != filepath.Join(f.prefix, "lib/php/pecl") {
		t.Errorf("pecl link = %q", got)
	}
	var keys []string
	for _, c := range f.ran("pear") {
		if c.Args[0] == "config-set" {
			keys = append(keys, c.Args[1])
		}
	}
	want := []string{"php_ini", "php_dir", "doc_dir", "ext_dir", "bin_dir", "data_dir", "cfg_dir", "www_dir", "man_dir", "test_dir", "php_bin"}
	if !slices.Equal(keys, want) {
		t.Errorf("config-set keys = %v, want %v", keys, want)
	}
	if !f.exists("share/pear@8.4/doc") || f.exists("share/man/x") {
		t.Error("the _dir paths were not made")
	}
	ini := f.read("etc/php/8.4/conf.d/ext-opcache.ini")
	if want := "[opcache]\nzend_extension=\"" + f.prefix + "/opt/php@8.4/lib/php/20240924/opcache.so\"\n"; ini != want {
		t.Errorf("ini = %q, want %q", ini, want)
	}
	f.write("etc/php/8.4/conf.d/ext-opcache.ini", "; hi\n zend_extension = old.so\n", 0o644)
	f.mustRun(`[{"type":"configure_php"}]`)
	if got := f.read("etc/php/8.4/conf.d/ext-opcache.ini"); !strings.Contains(got, "; hi\nzend_extension=\""+f.prefix) {
		t.Errorf("ini = %q", got)
	}
}

func TestBootstrapPyPyBootstrapsPackagesAndLinks(t *testing.T) {
	f := newSudoFixture(t)
	f.ctx.Name = "pypy3"
	f.mkdir("Cellar/pkg/1.2.3-1/bin")
	res := f.keg("libexec/post-install-resources")
	if err := os.MkdirAll(res, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, pkg := range []string{"setuptools", "pip"} {
		if err := os.WriteFile(filepath.Join(res, pkg+".tar.gz"), gzTar(t, pkg+"-1/setup.py", "x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	f.mustRun(`[{"type":"bootstrap_pypy","abi_version":"3.10"}]`)
	var probed, installed int
	for _, c := range f.ran("pypy3.10") {
		switch c.Args[0] {
		case "-c":
			probed++
		case "-s":
			installed++
			if !strings.HasPrefix(filepath.Base(c.Dir), "setuptools-1") && !strings.HasPrefix(filepath.Base(c.Dir), "pip-1") {
				t.Errorf("setup.py ran in %s", c.Dir)
			}
		}
	}
	if probed != 5 || installed != 2 {
		t.Errorf("probed %d modules, installed %d packages, want 5 and 2", probed, installed)
	}
	if !f.exists("lib/pypy3.10/site-packages/.keepme") {
		t.Error("site-packages was not made")
	}
	if got := f.read("Cellar/pkg/1.2.3-1/libexec/lib/pypy3.10/distutils/distutils.cfg"); got != "[install]\ninstall-scripts="+f.prefix+"/share/pypy3.10\n" {
		t.Errorf("distutils.cfg = %q", got)
	}
	for _, l := range []string{"bin/pip_pypy3.10", "bin/pip_pypy3"} {
		if !f.exists(l) {
			t.Errorf("%s was not linked", l)
		}
	}
}
