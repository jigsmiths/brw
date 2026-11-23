package rb

import (
	"fmt"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// Host does what an install body asks of the machine. Paths are absolute.
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
	// Run runs argv in dir with env added to the environment and returns its output.
	Run(dir string, env map[string]string, argv []string) ([]byte, error)
}

// InstallEnv is where an install body runs: the prefix it installs into and
// the staged download it starts in.
type InstallEnv struct {
	Host      Host
	Prefix    string
	Buildpath string
	Env       map[string]string // set by the body with ENV["X"] = v; nil starts empty
}

// buildFailure is an install body failing as brew's would, such as a command
// that exits non-zero, as opposed to Ruby brw cannot run.
type buildFailure struct{ err error }

func failBuild(f string, a ...any) { panic(buildFailure{fmt.Errorf(f, a...)}) }

// Install runs the formula's install method, as brew does once the download
// is staged: in buildpath, with prefix the keg it fills.
func (f *Formula) Install(env *InstallEnv) (err error) {
	d, ok := f.b.defs["install"]
	if !ok {
		return fmt.Errorf("%s: %w: no install method", f.b.name, ErrUnsupported)
	}
	if env.Env == nil {
		env.Env = map[string]string{}
	}
	defer func() {
		if r := recover(); r != nil {
			switch x := r.(type) {
			case unsupported:
				err = fmt.Errorf("%s: %w", f.b.name, x)
			case buildFailure:
				err = fmt.Errorf("%s: %w", f.b.name, x.err)
			case parseErr:
				err = fmt.Errorf("%s: %w: %s", f.b.name, ErrUnsupported, x.msg)
			default:
				panic(r)
			}
		}
	}()
	b := &builder{f: f.b, env: env, cwd: env.Buildpath}
	in := &interp{plat: f.b.plat, ctx: []dsl{&instanceDSL{f: f.b, root: env.Prefix}, b}, build: b}
	in.callDef(d, nil, nil, &scope{vars: map[string]value{}})
	return nil
}

// builder is the formula instance while install runs: FileUtils, system and
// the Pathname methods work on real files.
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

// globStar is Ruby's ** : any number of directories.
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

// installP is Pathname#install_p: move src into dir as name.
func (b *builder) installP(dir, src, name string) {
	from := b.abs(src)
	if _, err := b.env.Host.Lstat(from); err != nil {
		failBuild("no such file to install: %s", src)
	}
	b.must(b.env.Host.MkdirAll(dir))
	b.must(b.env.Host.Rename(from, filepath.Join(dir, name)))
}

// installSymlinkP is Pathname#install_symlink_p: a relative symlink in dir.
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

// eachSource calls fn with each file and its new name, as Pathname#install
// takes strings, arrays and renaming hashes.
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

// pathMethod is a Pathname method on a path while install runs.
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
		eachSource(args, func(src, n string) { b.installP(dir, src, n) })
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

// writeEnvScript is Pathname#write_env_script.
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

// run runs a command, failing the install when it fails, as brew's system does.
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

// shellParameter is Utils::ShellCompletion.completion_shell_parameter: what
// to add to the command, and the environment, for a shell's completions.
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

// completions is Formula#generate_completions_from_executable.
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

// fileUtils are the FileUtils methods a formula calls without a receiver.
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

// copy copies a file, or a directory as cp -r does.
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
		return rstr(b.env.Buildpath), true
	case c.name == "cd":
		if len(args) != 1 || c.block == nil {
			fail("cd without a block")
		}
		prev := b.cwd
		b.cwd = b.abs(str(args[0]))
		defer func() { b.cwd = prev }()
		return in.run(c.block.body, &scope{vars: map[string]value{}, parent: s}), true
	case c.name == "inreplace":
		b.inreplace(args, kw, c.block != nil)
	case fileUtils[c.name]:
		b.fileUtil(c.name, args)
	case c.name == "ohai" || c.name == "opoo" || c.name == "odebug":
	default:
		return nil, false
	}
	return nil, true
}

// inreplace is Utils::Inreplace.inreplace for its two-argument form.
func (b *builder) inreplace(args []value, kw *hash, block bool) {
	if block || len(args) != 3 {
		fail("inreplace with a block")
	}
	var paths []string
	if a, ok := args[0].(*arr); ok {
		for _, v := range a.v {
			paths = append(paths, str(v))
		}
	} else {
		paths = []string{str(args[0])}
	}
	for _, p := range paths {
		data, err := b.env.Host.ReadFile(b.abs(p))
		b.must(err)
		var out string
		if re, ok := args[1].(*regex); ok {
			out = compile(re).ReplaceAllString(string(data), rubySub(str(args[2])))
		} else {
			out = strings.ReplaceAll(string(data), str(args[1]), str(args[2]))
		}
		if out == string(data) && kwGet(kw, "audit_result") != rbool(false) {
			failBuild("inreplace failed: %s: %v did not match", p, args[1])
		}
		fi, err := b.env.Host.Lstat(b.abs(p))
		b.must(err)
		b.write(p, out, fi.Mode().Perm())
	}
}

// classMethod is File, Dir, FileUtils and Utils working on real files.
func (b *builder) classMethod(x special, name string, args []value) (value, bool) {
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
	if x == "FileUtils" && fileUtils[name] {
		b.fileUtil(name, args)
		return nil, true
	}
	return nil, false
}

// set is ENV["X"] = v.
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
