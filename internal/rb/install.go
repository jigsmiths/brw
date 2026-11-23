package rb

import (
	"cmp"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

type Host interface {
	Lstat(path string) (fs.FileInfo, error)
	ReadDir(path string) ([]fs.DirEntry, error)
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, perm fs.FileMode) error
	MkdirAll(path string) error
	Rename(oldpath, newpath string) error
	Symlink(target, link string) error
	Remove(path string) error
	RemoveAll(path string) error
	Chmod(path string, perm fs.FileMode) error
	Glob(pattern string) ([]string, error)
	Run(dir string, env map[string]string, argv []string) ([]byte, error)
}

type InstallEnv struct {
	Host      Host
	Prefix    string
	Buildpath string
	Env       map[string]string
	Tap       string
	Resources map[string]string
	Unpack    func(archive, dir string) error
	Scratch   string
	Out       io.Writer
	// TarCommit is the commit in the tarball header, which std_go_args(ldflags: :goreleaser) uses when the spec has no revision.
	TarCommit string
}

type buildFailure struct{ err error }

func failBuild(f string, a ...any) { panic(buildFailure{fmt.Errorf(f, a...)}) }

func (f *Formula) Install(env *InstallEnv) error { return f.run("install", env) }

func (f *Formula) PostInstall(env *InstallEnv) error { return f.run("post_install", env) }

func (f *Formula) run(method string, env *InstallEnv) (err error) {
	d, ok := f.b.defs[method]
	if !ok {
		return fmt.Errorf("%s: %w: no %s method", f.b.name, ErrUnsupported, method)
	}
	if env.Env == nil {
		env.Env = map[string]string{}
	}
	defer catch(f.b.name, &err)
	b := &builder{f: f.b, env: env, cwd: env.Buildpath}
	in := &interp{plat: f.b.plat, ctx: []dsl{&instanceDSL{f: f.b, root: env.Prefix}, b}, build: b}
	in.callDef(d, nil, nil, &scope{vars: map[string]value{}})
	return nil
}

type builder struct {
	f   *formulaB
	env *InstallEnv
	cwd string
}

func (b *builder) abs(p string) string {
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Join(b.cwd, p)
}

func (b *builder) must(err error) {
	if err != nil {
		failBuild("%v", err)
	}
}

func (b *builder) exists(p string) bool {
	_, err := b.env.Host.Lstat(b.abs(p))
	return err == nil
}

func (b *builder) isDir(p string) bool {
	fi, err := b.env.Host.Lstat(b.abs(p))
	return err == nil && fi.IsDir()
}

func (b *builder) glob(pattern string) *arr {
	out := &arr{}
	var matches []string
	if strings.Contains(pattern, "**") {
		matches = b.globStar(pattern)
	} else {
		m, err := b.env.Host.Glob(b.abs(pattern))
		b.must(err)
		matches = m
	}
	slices.Sort(matches)
	for _, m := range matches {
		if !filepath.IsAbs(pattern) {
			if rel, err := filepath.Rel(b.cwd, m); err == nil {
				m = rel
			}
		}
		out.v = append(out.v, rstr(m))
	}
	return out
}

func (b *builder) globStar(pattern string) []string {
	before, after, _ := strings.Cut(b.abs(pattern), "**")
	after = strings.TrimPrefix(after, "/")
	var out []string
	var walk func(dir string)
	walk = func(dir string) {
		m, err := b.env.Host.Glob(filepath.Join(dir, after))
		b.must(err)
		out = append(out, m...)
		entries, _ := b.env.Host.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
				walk(filepath.Join(dir, e.Name()))
			}
		}
	}
	walk(strings.TrimSuffix(before, "/"))
	return out
}

func (b *builder) installP(dir, src, name string) {
	from := b.abs(src)
	if _, err := b.env.Host.Lstat(from); err != nil {
		failBuild("no such file to install: %s", src)
	}
	b.must(b.env.Host.MkdirAll(dir))
	b.must(b.env.Host.Rename(from, filepath.Join(dir, name)))
}

func (b *builder) installSymlinkP(dir, src, name string) {
	b.must(b.env.Host.MkdirAll(dir))
	target := src
	if filepath.IsAbs(src) {
		rel, err := filepath.Rel(dir, src)
		b.must(err)
		target = rel
	}
	link := filepath.Join(dir, name)
	_ = b.env.Host.Remove(link)
	b.must(b.env.Host.Symlink(target, link))
}

func eachSource(args []value, fn func(src, name string)) {
	for _, a := range args {
		switch x := a.(type) {
		case *arr:
			for _, s := range x.v {
				fn(str(s), filepath.Base(str(s)))
			}
		case *hash:
			for i, k := range x.keys {
				fn(str(k), str(x.vals[i]))
			}
		case nil:
		default:
			fn(str(x), filepath.Base(str(x)))
		}
	}
}

func (b *builder) write(p, content string, perm fs.FileMode) {
	path := b.abs(p)
	b.must(b.env.Host.MkdirAll(filepath.Dir(path)))
	_ = b.env.Host.Remove(path)
	b.must(b.env.Host.WriteFile(path, []byte(content), perm))
}

func (b *builder) pathMethod(in *interp, p string, name string, args []value, blk *proc) (value, bool) {
	arg := func(i int) value {
		if i < len(args) {
			return args[i]
		}
		return nil
	}
	dir := b.abs(p)
	switch name {
	case "install":
		for _, a := range args {
			if r, ok := a.(resourceRef); ok {
				b.installResource(dir, r)
			} else {
				eachSource([]value{a}, func(src, n string) { b.installP(dir, src, n) })
			}
		}
	case "install_symlink":
		eachSource(args, func(src, n string) { b.installSymlinkP(dir, b.absFrom(dir, src), n) })
	case "mkpath":
		b.must(b.env.Host.MkdirAll(dir))
	case "write", "atomic_write":
		b.write(p, str(arg(0)), 0o644)
	case "append_lines":
		old, err := b.env.Host.ReadFile(dir)
		b.must(err)
		b.write(p, string(old)+str(arg(0))+"\n", 0o644)
	case "read":
		data, err := b.env.Host.ReadFile(dir)
		b.must(err)
		return rstr(data), true
	case "exist?":
		_, err := b.env.Host.Lstat(dir)
		return rbool(err == nil), true
	case "directory?":
		return rbool(b.isDir(p)), true
	case "file?":
		fi, err := b.env.Host.Lstat(dir)
		return rbool(err == nil && fi.Mode().IsRegular()), true
	case "symlink?":
		fi, err := b.env.Host.Lstat(dir)
		return rbool(err == nil && fi.Mode()&fs.ModeSymlink != 0), true
	case "children":
		entries, err := b.env.Host.ReadDir(dir)
		b.must(err)
		out := &arr{}
		for _, e := range entries {
			out.v = append(out.v, rstr(filepath.Join(p, e.Name())))
		}
		return out, true
	case "glob":
		return b.glob(filepath.Join(dir, str(arg(0)))), true
	case "chmod":
		mode, ok := arg(0).(rint)
		if !ok {
			fail("chmod %v", arg(0))
		}
		b.must(b.env.Host.Chmod(dir, fs.FileMode(mode)))
	case "unlink", "delete":
		b.must(b.env.Host.Remove(dir))
	case "rmtree":
		b.must(b.env.Host.RemoveAll(dir))
	case "write_exec_script":
		b.must(b.env.Host.MkdirAll(dir))
		eachSource(args, func(target, n string) {
			b.write(filepath.Join(dir, n), "#!/bin/bash\nexec \""+b.abs(target)+"\" \"$@\"\n", 0o644)
		})
	case "write_env_script":
		b.writeEnvScript(p, args)
	case "make_relative_symlink":
		rel, err := filepath.Rel(filepath.Dir(dir), b.abs(str(arg(0))))
		b.must(err)
		b.must(b.env.Host.MkdirAll(filepath.Dir(dir)))
		b.must(b.env.Host.Symlink(rel, dir))
	case "realpath", "expand_path":
		return rstr(dir), true
	case "parent", "dirname":
		return rstr(filepath.Dir(p)), true
	case "basename":
		return rstr(filepath.Base(p)), true
	case "extname":
		return rstr(filepath.Ext(p)), true
	case "relative_path_from":
		rel, err := filepath.Rel(b.abs(str(arg(0))), dir)
		b.must(err)
		return rstr(rel), true
	default:
		return nil, false
	}
	return nil, true
}

func (b *builder) absFrom(dir, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(dir, p)
}

func (b *builder) writeEnvScript(p string, args []value) {
	if len(args) < 2 {
		fail("write_env_script")
	}
	target := b.abs(str(args[0]))
	var env *hash
	var extra string
	switch x := args[1].(type) {
	case *hash:
		env = x
		if len(args) > 2 {
			fail("write_env_script with args and env")
		}
	case *arr:
		var parts []string
		for _, v := range x.v {
			parts = append(parts, str(v))
		}
		extra = strings.Join(parts, " ")
		env, _ = args[2].(*hash)
	default:
		extra = str(x)
		env, _ = args[2].(*hash)
	}
	if env == nil {
		fail("write_env_script without an env hash")
	}
	var exports strings.Builder
	for i, k := range env.keys {
		fmt.Fprintf(&exports, "%s=\"%s\" ", str(k), str(env.vals[i]))
	}
	b.write(p, "#!/bin/bash\n"+exports.String()+"exec \""+target+"\" "+extra+" \"$@\"\n", 0o644)
	b.must(b.env.Host.Chmod(b.abs(p), 0o555))
}

func (b *builder) run(env map[string]string, args []value) []byte {
	var argv []string
	for _, a := range args {
		switch x := a.(type) {
		case *arr:
			for _, v := range x.v {
				argv = append(argv, str(v))
			}
		case *hash:
			fail("command options")
		default:
			argv = append(argv, str(x))
		}
	}
	if len(argv) == 0 {
		fail("empty command")
	}
	merged := map[string]string{}
	maps.Copy(merged, b.env.Env)
	maps.Copy(merged, env)
	out, err := b.env.Host.Run(b.cwd, merged, argv)
	if err != nil {
		failBuild("failed executing: %s: %v", strings.Join(argv, " "), err)
	}
	return out
}

func shellParameter(format value, shell, executable string, env map[string]string) []string {
	param := shell
	if shell == "pwsh" {
		param = "powershell"
	}
	switch format {
	case nil:
		return []string{param}
	case sym("arg"):
		return []string{"--shell=" + param}
	case sym("clap"):
		env["COMPLETE"] = param
		return nil
	case sym("click"):
		prog := strings.ToUpper(strings.ReplaceAll(filepath.Base(executable), "-", "_"))
		env["_"+prog+"_COMPLETE"] = param + "_source"
		return nil
	case sym("cobra"):
		return []string{"completion", param}
	case sym("flag"):
		return []string{"--" + param}
	case sym("none"):
		return nil
	case sym("typer"):
		env["_TYPER_COMPLETE_TEST_DISABLE_SHELL_DETECTION"] = "1"
		return []string{"--show-completion", param}
	}
	return []string{str(format) + shell}
}

func (b *builder) completions(in *interp, args []value, kw *hash) {
	if len(args) == 0 {
		fail("generate_completions_from_executable without a command")
	}
	executable := str(args[0])
	format := kwGet(kw, "shell_parameter_format")
	shells := []string{"bash", "zsh", "fish"}
	if format == sym("cobra") || format == sym("typer") {
		shells = append(shells, "pwsh")
	}
	if s := kwGet(kw, "shells"); s != nil {
		shells = symList(s)
	}
	inst := &instanceDSL{f: b.f, root: b.env.Prefix}
	path := func(m string) string {
		p, _ := formulaPath(inst.prefix(), "", b.f.name, b.env.Prefix, m)
		return p
	}
	if format != nil {
		if _, isSym := format.(sym); !isSym {
			format = rstr(str(format))
		}
	}
	base := str(kwGet(kw, "base_name"))
	if base == "" && (strings.HasPrefix(executable, path("bin")) || strings.HasPrefix(executable, path("sbin"))) {
		base = filepath.Base(executable)
	}
	if base == "" {
		base = b.f.name
	}
	scripts := map[string]string{
		"bash": filepath.Join(path("bash_completion"), base),
		"zsh":  filepath.Join(path("zsh_completion"), "_"+base),
		"fish": filepath.Join(path("fish_completion"), base+".fish"),
		"pwsh": filepath.Join(path("pwsh_completion"), "_"+base+".ps1"),
	}
	for _, shell := range shells {
		script, ok := scripts[shell]
		if !ok {
			fail("completions for %s", shell)
		}
		env := map[string]string{"SHELL": shell}
		cmd := slices.Clone(args)
		for _, p := range shellParameter(format, shell, executable, env) {
			cmd = append(cmd, rstr(p))
		}
		out := b.run(env, cmd)
		b.write(script, string(out), 0o644)
	}
}

var fileUtils = map[string]bool{
	"cp": true, "cp_r": true, "mv": true, "rm": true, "rm_f": true, "rm_r": true, "rm_rf": true, "rmtree": true,
	"mkdir": true, "mkdir_p": true, "ln_s": true, "ln_sf": true, "touch": true, "chmod": true, "rmdir": true,
}

func (b *builder) fileUtil(name string, args []value) {
	if len(args) == 0 {
		fail("%s without arguments", name)
	}
	var srcs []string
	last := args[len(args)-1]
	for _, a := range args {
		if x, ok := a.(*arr); ok {
			for _, v := range x.v {
				srcs = append(srcs, str(v))
			}
			continue
		}
		srcs = append(srcs, str(a))
	}
	h := b.env.Host
	switch name {
	case "rm", "rm_f", "rm_r", "rm_rf", "rmtree", "rmdir":
		for _, p := range srcs {
			var err error
			if name == "rm" || name == "rm_f" || name == "rmdir" {
				err = h.Remove(b.abs(p))
			} else {
				err = h.RemoveAll(b.abs(p))
			}
			if err != nil && (name == "rm" || name == "rmdir") {
				failBuild("%s %s: %v", name, p, err)
			}
		}
	case "mkdir", "mkdir_p":
		for _, p := range srcs {
			b.must(h.MkdirAll(b.abs(p)))
		}
	case "touch":
		for _, p := range srcs {
			if !b.exists(p) {
				b.write(p, "", 0o644)
			}
		}
	case "chmod":
		mode, ok := args[0].(rint)
		if !ok {
			fail("chmod %v", args[0])
		}
		for _, p := range srcs[1:] {
			b.must(h.Chmod(b.abs(p), fs.FileMode(mode)))
		}
	case "mv", "cp", "cp_r", "ln_s", "ln_sf":
		if len(srcs) < 2 {
			fail("%s needs a source and a target", name)
		}
		dst := b.abs(str(last))
		from := srcs[:len(srcs)-1]
		for _, src := range from {
			to := dst
			if b.isDir(str(last)) {
				to = filepath.Join(dst, filepath.Base(src))
			}
			switch name {
			case "mv":
				b.must(h.Rename(b.abs(src), to))
			case "cp", "cp_r":
				b.copy(b.abs(src), to)
			default:
				if name == "ln_sf" {
					_ = h.Remove(to)
				}
				b.must(h.Symlink(src, to))
			}
		}
	}
}

func (b *builder) copy(src, dst string) {
	h := b.env.Host
	fi, err := h.Lstat(src)
	b.must(err)
	switch {
	case fi.IsDir():
		b.must(h.MkdirAll(dst))
		entries, err := h.ReadDir(src)
		b.must(err)
		for _, e := range entries {
			b.copy(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()))
		}
	case fi.Mode()&fs.ModeSymlink != 0:
		fail("copying a symlink")
	default:
		data, err := h.ReadFile(src)
		b.must(err)
		b.must(h.MkdirAll(filepath.Dir(dst)))
		b.must(h.WriteFile(dst, data, fi.Mode().Perm()))
	}
}

func (b *builder) call(in *interp, s *scope, c *callN, args []value, kw *hash) (value, bool) {
	switch {
	case c.name == "system":
		if kw != nil {
			args = append(args, kw)
		}
		b.run(nil, args)
	case c.name == "generate_completions_from_executable":
		b.completions(in, args, kw)
	case c.name == "buildpath":
		return pathV(b.env.Buildpath), true
	case c.name == "cd":
		if len(args) != 1 || c.block == nil {
			fail("cd without a block")
		}
		prev := b.cwd
		b.cwd = b.abs(str(args[0]))
		defer func() { b.cwd = prev }()
		return in.run(c.block.body, &scope{vars: map[string]value{}, parent: s}), true
	case c.name == "inreplace" && c.block != nil:
		b.inreplaceBlock(in, s, args, c.block)
	case c.name == "inreplace":
		b.inreplace(args, kw)
	case strings.HasPrefix(c.name, "std_") && strings.HasSuffix(c.name, "_args"):
		return b.stdArgs(c.name, kw), true
	case c.name == "time":
		return rstr(b.time().Format(time.RFC3339)), true
	case c.name == "build" && len(args) == 0:
		return buildOptions{b.f}, true
	case c.name == "resource" && len(args) == 1:
		return b.resource(str(args[0])), true
	case c.name == "resources":
		out := &arr{}
		if sp := b.f.active(); sp != nil {
			for _, r := range sp.resource {
				out.v = append(out.v, resourceRef{r})
			}
		}
		return out, true
	case fileUtils[c.name]:
		b.fileUtil(c.name, args)
	case c.name == "ohai" || c.name == "opoo" || c.name == "puts":
		if b.env.Out != nil {
			lead := map[string]string{"ohai": "==> ", "opoo": "Warning: "}[c.name]
			for _, a := range args {
				_, _ = fmt.Fprintf(b.env.Out, "%s%s\n", lead, str(a))
			}
		}
	case c.name == "odebug":
	default:
		return nil, false
	}
	return nil, true
}

func (b *builder) inreplace(args []value, kw *hash) {
	if len(args) != 3 {
		fail("inreplace with %d arguments", len(args))
	}
	edit := "gsub!"
	if kwGet(kw, "global") == rbool(false) {
		edit = "sub!"
	}
	audit := kwGet(kw, "audit_result") != rbool(false)
	b.eachInreplace(args[:1], func(buf *buffer) { buf.edit(edit, args[1], args[2], audit) })
}

func (b *builder) classMethod(in *interp, x special, name string, args []value, blk *proc) (value, bool) {
	arg := func(i int) string {
		if i >= len(args) {
			fail("%s.%s needs %d arguments", x, name, i+1)
		}
		return str(args[i])
	}
	switch string(x) + "." + name {
	case "File.exist?", "File.directory?", "File.file?", "File.symlink?", "File.read", "File.unlink":
		return b.pathMethod(nil, arg(0), name, nil, nil)
	case "Dir.exist?":
		return b.pathMethod(nil, arg(0), "directory?", nil, nil)
	case "File.delete":
		return b.pathMethod(nil, arg(0), "unlink", nil, nil)
	case "File.write":
		return b.pathMethod(nil, arg(0), "write", args[1:], nil)
	case "File.chmod":
		return b.pathMethod(nil, arg(1), "chmod", args[:1], nil)
	case "File.join":
		var parts []string
		for i := range args {
			parts = append(parts, arg(i))
		}
		return rstr(filepath.Join(parts...)), true
	case "File.basename":
		base := filepath.Base(arg(0))
		if len(args) > 1 {
			base = strings.TrimSuffix(base, arg(1))
		}
		return rstr(base), true
	case "File.dirname":
		return rstr(filepath.Dir(arg(0))), true
	case "File.expand_path":
		if p := arg(0); p == "~" || strings.HasPrefix(p, "~/") {
			return rstr(filepath.Clean(b.env.Env["HOME"] + p[1:])), true
		}
		return rstr(b.abs(arg(0))), true
	case "Dir.glob":
		return b.glob(arg(0)), true
	case "Dir.pwd":
		return rstr(b.cwd), true
	case "Utils.safe_popen_read", "Utils.popen_read":
		var env map[string]string
		if h, ok := args[0].(*hash); ok {
			env = map[string]string{}
			for i, k := range h.keys {
				env[str(k)] = str(h.vals[i])
			}
			args = args[1:]
		}
		return rstr(b.run(env, args)), true
	case "ENV.fetch":
		if v, ok := b.env.Env[arg(0)]; ok {
			return rstr(v), true
		}
		return nil, false
	}
	if x == "ENV" {
		return b.envMethod(in, name, args, blk)
	}
	if x == "FileUtils" && fileUtils[name] {
		b.fileUtil(name, args)
		return nil, true
	}
	return nil, false
}

type buildOptions struct{ f *formulaB }

func (buildOptions) rubyValue() {}

func (o buildOptions) defined(flag string) bool {
	for _, opt := range o.f.options {
		if v, _ := opt.get(rstr("option")); str(v) == flag {
			return true
		}
	}
	return false
}

func (o buildOptions) method(name string, args []value) value {
	with := func() bool {
		if len(args) != 1 {
			fail("build.%s", name)
		}
		n := strings.TrimPrefix(str(args[0]), "--")
		return !o.defined("--with-"+n) && o.defined("--without-"+n)
	}
	switch name {
	case "with?":
		return rbool(with())
	case "without?":
		return rbool(!with())
	case "bottle?", "include?", "any_args_or_options?":
		return rbool(false)
	case "head?":
		return rbool(o.f.installHead)
	case "stable?":
		return rbool(!o.f.installHead)
	case "used_options", "unused_options", "args":
		return &arr{}
	}
	fail("build.%s", name)
	return nil
}

func (b *builder) time() time.Time {
	if n, err := strconv.ParseInt(b.env.Env["SOURCE_DATE_EPOCH"], 10, 64); err == nil {
		return time.Unix(n, 0).UTC()
	}
	return time.Now().UTC()
}

func (b *builder) prefixPath(m string) string {
	inst := &instanceDSL{f: b.f, root: b.env.Prefix}
	p, _ := formulaPath(inst.prefix(), "", b.f.name, b.env.Prefix, m)
	return p
}

func list(vs ...string) *arr { return strs(vs) }

func joined(v value, sep string) string {
	if a, ok := v.(*arr); ok {
		var parts []string
		for _, e := range a.v {
			parts = append(parts, str(e))
		}
		return strings.Join(parts, sep)
	}
	return str(v)
}

func (b *builder) stdArgs(name string, kw *hash) value {
	or := func(k, def string) string {
		if v := kwGet(kw, k); v != nil {
			return str(v)
		}
		return def
	}
	prefix := b.prefixPath("prefix")
	switch name {
	case "std_configure_args":
		p := or("prefix", prefix)
		libdir := or("libdir", "lib")
		if !filepath.IsAbs(libdir) {
			libdir = filepath.Join(p, libdir)
		}
		return list("--disable-debug", "--disable-dependency-tracking", "--prefix="+p, "--libdir="+libdir)
	case "std_cmake_args":
		args := []string{"-DCMAKE_INSTALL_PREFIX=" + or("install_prefix", prefix), "-DCMAKE_INSTALL_LIBDIR=" + or("install_libdir", "lib"),
			"-DCMAKE_BUILD_TYPE=Release", "-DCMAKE_FIND_FRAMEWORK=" + or("find_framework", "LAST"), "-DCMAKE_VERBOSE_MAKEFILE=ON"}
		trap := filepath.Join(b.env.Prefix, "Library", "Homebrew", "cmake", "trap_fetchcontent_provider.cmake")
		if _, err := b.env.Host.Lstat(trap); err == nil {
			args = append(args, "-DCMAKE_PROJECT_TOP_LEVEL_INCLUDES="+trap)
		}
		return list(append(args, "-Wno-author", "-DBUILD_TESTING=OFF", "-DCCACHE_FOUND=OFF")...)
	case "std_go_args":
		ldflags := kwGet(kw, "ldflags")
		if ldflags == sym("goreleaser") {
			builtBy, _, _ := strings.Cut(b.env.Tap, "/")
			if builtBy == "" {
				builtBy = "Homebrew"
			}
			commit := cmp.Or(b.env.TarCommit, builtBy)
			if r := kwGet(b.f.active().specs, "revision"); r != nil {
				commit = str(r)
			}
			ldflags = list("-X 'main.version="+b.f.version()+"'", "-X 'main.commit="+commit+"'",
				"-X 'main.date="+b.time().Format(time.RFC3339)+"'", "-X 'main.builtBy="+builtBy+"'")
		}
		flags := "-s -w"
		if ldflags != nil {
			flags += " " + joined(ldflags, " ")
		}
		out := []string{"-trimpath", "-o=" + or("output", filepath.Join(b.prefixPath("bin"), b.f.name))}
		if t := kwGet(kw, "tags"); t != nil {
			out = append(out, "-tags="+joined(t, ","))
		}
		out = append(out, "-ldflags="+flags)
		if g := kwGet(kw, "gcflags"); g != nil {
			out = append(out, "-gcflags="+joined(g, " "))
		}
		return list(out...)
	case "std_cargo_args":
		args := []string{"--jobs", strconv.Itoa(makeJobCount()), "--locked", "--root=" + or("root", prefix), "--path=" + or("path", ".")}
		if f := kwGet(kw, "features"); f != nil {
			args = append(args, "--features="+joined(f, ","))
		}
		return list(args...)
	case "std_meson_args":
		return list("--prefix="+or("prefix", prefix), "--libdir="+or("libdir", "lib"), "--buildtype=release", "--wrap-mode=nofallback")
	case "std_swift_args":
		return list("--configuration", "release", "--jobs", strconv.Itoa(makeJobCount()))
	}
	fail("%s", name)
	return nil
}

var ccFlagVars = []string{"CFLAGS", "CXXFLAGS", "OBJCFLAGS", "OBJCXXFLAGS"}

func (b *builder) envMethod(in *interp, name string, args []value, blk *proc) (value, bool) {
	env := b.env.Env
	arg := func(i int) string {
		if i >= len(args) {
			fail("ENV.%s needs %d arguments", name, i+1)
		}
		return str(args[i])
	}
	keys := func() []string {
		if a, ok := args[0].(*arr); ok {
			var out []string
			for _, v := range a.v {
				out = append(out, str(v))
			}
			return out
		}
		return []string{arg(0)}
	}
	join := func(keys []string, val, sep string, front bool) {
		for _, key := range keys {
			switch {
			case strings.TrimSpace(env[key]) == "":
				env[key] = val
			case front:
				env[key] = val + sep + env[key]
			default:
				env[key] = env[key] + sep + val
			}
		}
	}
	path := func(key string, front bool, p string) {
		var parts []string
		if front {
			parts = append(strings.Split(p, ":"), strings.Split(env[key], ":")...)
		} else {
			parts = append(strings.Split(env[key], ":"), strings.Split(p, ":")...)
		}
		var out []string
		for _, s := range parts {
			if s != "" && !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
		env[key] = strings.Join(out, ":")
	}
	remove := func(keys []string, re *regexp.Regexp) {
		for _, k := range keys {
			v, ok := env[k]
			if !ok {
				continue
			}
			if loc := re.FindStringIndex(v); loc != nil {
				v = v[:loc[0]] + v[loc[1]:]
			}
			if v == "" {
				delete(env, k)
			} else {
				env[k] = v
			}
		}
	}
	sep := func() string {
		if len(args) > 2 {
			return arg(2)
		}
		return " "
	}
	switch name {
	case "append", "prepend":
		join(keys(), arg(1), sep(), name == "prepend")
	case "append_path":
		path(arg(0), false, arg(1))
	case "prepend_path", "prepend_create_path":
		if name == "prepend_create_path" {
			b.must(b.env.Host.MkdirAll(b.abs(arg(1))))
		}
		if !slices.Contains([]string{"/usr/bin", "/bin", "/usr/sbin", "/sbin"}, arg(1)) {
			path(arg(0), true, arg(1))
		}
	case "delete":
		delete(env, arg(0))
	case "deparallelize":
		old, had := env["MAKEFLAGS"]
		remove([]string{"MAKEFLAGS"}, regexp.MustCompile(`-j\d+`))
		if blk != nil {
			defer func() {
				if had {
					env["MAKEFLAGS"] = old
				} else {
					delete(env, "MAKEFLAGS")
				}
			}()
			return in.callBlock(blk), true
		}
	case "make_jobs":
		return rint(makeJobCount()), true
	case "cxx11":
		join([]string{"CXX"}, "-std=c++11", " ", false)
		join([]string{"CXX"}, "-stdlib=libc++", " ", false)
	case "libcxx":
		join([]string{"CXX"}, "-stdlib=libc++", " ", false)
	case "cc", "cxx":
		return rstr(env[strings.ToUpper(name)]), true
	case "O0", "O1":
		remove(ccFlagVars, regexp.MustCompile(`-O.`))
		join(ccFlagVars, "-"+name, " ", false)
	default:
		return nil, false
	}
	return nil, true
}

func makeJobCount() int {
	if s, ok := lookupEnv("HOMEBREW_MAKE_JOBS"); ok {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			return n
		}
	}
	return runtime.NumCPU()
}

type buffer struct {
	s      string
	errors []string
}

func (*buffer) rubyValue() {}

func inspect(v value) string {
	if re, ok := v.(*regex); ok {
		return "/" + re.src + "/"
	}
	return strconv.Quote(str(v))
}

func (x *buffer) replace(before, after value, all bool) bool {
	if re, ok := before.(*regex); ok {
		r := compile(re)
		if all {
			matched := r.MatchString(x.s)
			x.s = r.ReplaceAllString(x.s, rubySub(str(after)))
			return matched
		}
		loc := r.FindStringSubmatchIndex(x.s)
		if loc == nil {
			return false
		}
		out := r.ExpandString(nil, rubySub(str(after)), x.s, loc)
		x.s = x.s[:loc[0]] + string(out) + x.s[loc[1]:]
		return true
	}
	if !strings.Contains(x.s, str(before)) {
		return false
	}
	n := -1
	if !all {
		n = 1
	}
	x.s = strings.Replace(x.s, str(before), str(after), n)
	return true
}

func (x *buffer) edit(name string, before, after value, audit bool) value {
	if !x.replace(before, after, name == "gsub!") {
		if audit {
			x.errors = append(x.errors, "expected replacement of "+inspect(before)+" with "+inspect(after))
		}
		return nil
	}
	return x
}

func makeVar(flag, tail string) *regex {
	return &regex{src: `^` + regexp.QuoteMeta(flag) + `[ \t]*[\\?+:!]?=` + tail}
}

const makeValue = `[ \t]*((?:.*\\\n)*.*)$`

func bufferMethod(x *buffer, name string, args []value) value {
	audit := true
	if n := len(args); n > 0 {
		if h, ok := args[n-1].(*hash); ok {
			audit = kwGet(h, "audit_result") != rbool(false)
			args = args[:n-1]
		}
	}
	switch name {
	case "gsub!", "sub!":
		if len(args) != 2 {
			fail("%s with a block", name)
		}
		return x.edit(name, args[0], args[1], audit)
	case "change_make_var!":
		flag := str(args[0])
		if !x.replace(makeVar(flag, makeValue), rstr(flag+"="+str(args[1])), true) {
			x.errors = append(x.errors, "expected to change "+inspect(args[0])+" to "+inspect(args[1]))
		}
		return nil
	case "remove_make_var!":
		var flags []value
		if a, ok := args[0].(*arr); ok {
			flags = a.v
		} else {
			flags = args[:1]
		}
		for _, f := range flags {
			if !x.replace(makeVar(str(f), `(?:.*\\\n)*.*$\n?`), rstr(""), true) {
				x.errors = append(x.errors, "expected to remove "+inspect(f))
			}
		}
		return nil
	case "get_make_var":
		m := compile(makeVar(str(args[0]), makeValue)).FindStringSubmatch(x.s)
		if m == nil {
			failBuild("expected to find make variable %s", inspect(args[0]))
		}
		return rstr(m[1])
	case "to_s", "to_str":
		return rstr(x.s)
	}
	return strMethod(x.s, name, args)
}

func (b *builder) inreplaceBlock(in *interp, s *scope, args []value, blk *blockN) {
	b.eachInreplace(args, func(buf *buffer) { in.callBlock(&proc{b: blk, env: s}, buf) })
}

func (b *builder) eachInreplace(args []value, edit func(*buffer)) {
	var paths []string
	for _, a := range args {
		if x, ok := a.(*arr); ok {
			for _, v := range x.v {
				paths = append(paths, str(v))
			}
		} else {
			paths = append(paths, str(a))
		}
	}
	var errs []string
	for _, p := range paths {
		data, err := b.env.Host.ReadFile(b.abs(p))
		b.must(err)
		buf := &buffer{s: string(data)}
		edit(buf)
		for _, e := range buf.errors {
			errs = append(errs, p+": "+e)
		}
		fi, err := b.env.Host.Lstat(b.abs(p))
		b.must(err)
		b.write(p, buf.s, fi.Mode().Perm())
	}
	if len(errs) > 0 {
		failBuild("inreplace failed\n%s", strings.Join(errs, "\n"))
	}
}

func (b *builder) set(in *interp, target node, v value, s *scope) bool {
	ix, ok := target.(*indexN)
	if !ok || len(ix.args) != 1 {
		return false
	}
	if c, ok := ix.recv.(*constN); !ok || strings.Join(c.path, "::") != "ENV" {
		return false
	}
	key := str(in.eval(ix.args[0], s))
	if v == nil {
		delete(b.env.Env, key)
	} else {
		b.env.Env[key] = str(v)
	}
	return true
}
