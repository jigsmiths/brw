package poststep

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jigsmiths/brw/internal/sandbox"
	"github.com/jigsmiths/brw/internal/sudo"
	"github.com/jigsmiths/brw/internal/trust"
)

type SandboxMode int

const (
	Sandboxed SandboxMode = iota + 1
	Unsandboxed
)

const ChildCommand = "__sandboxed-steps"

const (
	errorPipeEnv       = "HOMEBREW_ERROR_PIPE"
	messageChannelEnv  = "HOMEBREW_CHILD_MESSAGE_CHANNEL"
	avoidNestedEnv     = "HOMEBREW_AVOID_NESTED_SANDBOXING"
	privilegedRequest  = "homebrew_cask_privileged_step"
	privilegedSucceed  = "homebrew_cask_privileged_step_succeeded"
	socketName         = "socket"
	payloadName        = "payload.json"
	scratchRoot        = "var/homebrew/sandbox"
	scratchHomeName    = "home"
	tempCellar         = "var/homebrew/tmp/.cellar"
	locksDir           = "var/homebrew/locks"
	caskStepName       = "running cask artifact operations"
	postInstallStep    = "running post-install"
	sandboxDirPrefix   = "sandbox"
	randomNameBytes    = 6
	disclaimUnavail    = "macOS privacy permission isolation is unavailable.\nContinuing with the existing sandbox restrictions."
	interruptExitCode  = 130
	installStepsAction = "install_steps"
	formulaAction      = "formula_steps"
)

var ciPathVariables = []string{"GITHUB_WORKSPACE", "RUNNER_WORKSPACE", "RUNNER_TEMP"}

const (
	maxMessageBytes     = 1 << 20
	homebrewPathEnv     = "HOMEBREW_PATH"
	bazelrcName         = ".bazelrc"
	releaseCooldownDays = "1"
	releaseCooldownAge  = "1 day"
	postinstallNetwork  = "HOMEBREW_FORMULA_POSTINSTALL_NETWORK"
	networkAllow        = "allow"
)

var kegLinkDirectories = []string{"bin", "etc", "include", "lib", "sbin", "share", "var"}

var defaultPrefixes = []string{"/opt/homebrew", "/usr/local", "/home/linuxbrew/.linuxbrew"}

var privilegedGroups = []string{"admin", "staff", "root", "wheel"}

var sensitiveName = regexp.MustCompile(`(?i)(cookie|key|token|password|passphrase|auth)`)

type SandboxPolicy struct {
	Cache         string
	Logs          string
	Repository    string
	BrewFile      string
	Prefix        string
	Child         []string
	Stdin         io.Reader
	PassStdin     bool
	Verbose       bool
	NetworkAccess bool
	AvoidNested   bool
	DefaultPrefix bool
	Available     func() bool
	Nested        func() bool
	Inherited     func() (bool, error)
	Warn          func(message string)
}

func NewSandboxPolicy(prefix, cache, logs, repository string, child []string, stdin io.Reader, verbose bool, warn func(message string)) SandboxPolicy {
	p := SandboxPolicy{
		Cache: cache, Logs: logs, Repository: repository, Prefix: prefix, BrewFile: filepath.Join(prefix, "bin/brew"),
		Child: child, Stdin: stdin, PassStdin: stdin != nil, Verbose: verbose, Warn: warn,
		NetworkAccess: postinstallNetworkAllowed(),
		AvoidNested:   sudo.EnvTrue(avoidNestedEnv),
		DefaultPrefix: slices.Contains(defaultPrefixes, prefix),
		Available:     sandbox.Available, Nested: sandbox.Nested,
	}
	p.Inherited = func() (bool, error) { return sandbox.Inherited(child[0], p.BrewFile) }
	return p
}

// Unset means allow: API formulae carry no DSL to say otherwise.
func postinstallNetworkAllowed() bool {
	if v := os.Getenv(postinstallNetwork); v != "" {
		return v == networkAllow
	}
	return true
}

func privilegedGroup() sandbox.PrivilegedGroup {
	u, err := user.Current()
	if err != nil {
		return sandbox.PrivilegedGroup{}
	}
	ids, _ := u.GroupIds()
	for _, name := range privilegedGroups {
		if g, err := user.LookupGroup(name); err == nil && slices.Contains(ids, g.Gid) {
			return sandbox.PrivilegedGroup{Name: name, Member: true}
		}
	}
	return sandbox.PrivilegedGroup{}
}

func (r *runner) sandboxStep() string {
	if r.c.cask() {
		return caskStepName
	}
	return postInstallStep
}

func (r *runner) useSandbox() (bool, error) {
	p := r.c.SandboxPolicy
	inherited, err := p.Inherited()
	if err != nil {
		return false, err
	}
	o := sandbox.Options{Inherited: inherited, Available: p.Available(), AvoidNested: p.AvoidNested, DefaultPrefix: p.DefaultPrefix, Prefix: p.Prefix}
	if p.AvoidNested {
		o.Nested = p.Nested()
		o.PrivilegedGroup = privilegedGroup()
	}
	d, message, err := sandbox.Decide(r.sandboxStep(), o)
	if err != nil {
		return false, err
	}
	if message != "" {
		p.Warn(message)
	}
	return d == sandbox.Use, nil
}

type contextPayload struct {
	Prefix  string `json:"prefix"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Keg     string `json:"keg"`
	OS      string `json:"os"`
	User    string `json:"user"`
	Token   string `json:"token"`
	Staged  string `json:"staged_path"`
	Arch    string `json:"arch"`
	AppDir  string `json:"appdir"`
	Host    Host   `json:"host"`
}

type payload struct {
	Action  string            `json:"action"`
	Context contextPayload    `json:"context"`
	Extra   json.RawMessage   `json:"extra,omitempty"`
	Phase   phase             `json:"phase"`
	Steps   []json.RawMessage `json:"steps"`
}

func (r *runner) stepsAction() string {
	if r.c.cask() {
		return installStepsAction
	}
	return formulaAction
}

func (r *runner) payload(action string, extra json.RawMessage, raw []json.RawMessage, ph phase) payload {
	c := r.c
	return payload{Action: action, Extra: extra, Phase: ph, Steps: raw, Context: contextPayload{
		Prefix: c.Prefix, Name: c.Name, Version: c.Version, Keg: c.Keg, OS: c.OS, User: c.User, Token: c.Token,
		Staged: c.Staged, Arch: c.Arch, AppDir: c.AppDir, Host: c.Host,
	}}
}

func randomName() (string, error) {
	b := make([]byte, randomNameBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

type childResult struct {
	mu         sync.Mutex
	reported   bool
	errData    string
	handlerErr error
}

func (c *childResult) setError(line string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.reported {
		c.reported, c.errData = true, line
	}
}

func (c *childResult) setHandler(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.handlerErr == nil {
		c.handlerErr = err
	}
}

func (r *runner) runInSandbox(raw []json.RawMessage, ph phase) (bool, error) {
	use, err := r.useSandbox()
	if err != nil || !use {
		return false, err
	}
	r.c.SandboxPolicy.Warn(disclaimUnavail)
	return true, r.runChild(r.stepsAction(), nil, raw, ph)
}

func RunAction(ctx context.Context, c Context, action string, extra json.RawMessage, inProcess func() error) error {
	if c.Sandbox != Sandboxed && c.Sandbox != Unsandboxed {
		return errors.New("the context does not say whether steps are sandboxed")
	}
	if c.Sandbox == Sandboxed && c.OS == "darwin" {
		r := &runner{ctx: ctx, c: c, guards: map[string]bool{}}
		use, err := r.useSandbox()
		if err != nil {
			return err
		}
		if use {
			c.SandboxPolicy.Warn(disclaimUnavail)
			return r.runChild(action, extra, nil, phaseInstall)
		}
	}
	return inProcess()
}

func (r *runner) runChild(action string, extra json.RawMessage, raw []json.RawMessage, ph phase) error {
	c, policy := r.c, r.c.SandboxPolicy
	tmp, err := os.MkdirTemp(c.Temp, sandboxDirPrefix) //fsx:allow brew's Mktemp for the sandbox, outside the prefix
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }() //fsx:allow brew's Mktemp for the sandbox, outside the prefix
	home, payloadPath, cleanup, err := r.scratch(r.payload(action, extra, raw, ph), tmp)
	if err != nil {
		return err
	}
	defer cleanup()
	body, err := os.ReadFile(payloadPath)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(body)

	sb, err := r.profile(raw, ph, home, payloadPath, tmp)
	if err != nil {
		return err
	}
	socketPath := filepath.Join(tmp, socketName)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	result := &childResult{}
	var served sync.WaitGroup
	served.Go(func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		r.serve(conn, raw, ph, result)
	})

	needsParent := slices.ContainsFunc(raw, func(b json.RawMessage) bool { var s step; return json.Unmarshal(b, &s) == nil && s.requiresSudo(true) })
	spec := sandbox.Spec{
		Profile: sb.Profile(), Exe: policy.Child[0], Args: append(slices.Clone(policy.Child[1:]), payloadPath, hex.EncodeToString(sum[:])),
		Env: r.childEnv(home, tmp, socketPath), Dir: tmp, Stdin: policy.Stdin, PassStdin: policy.PassStdin && !needsParent,
		Stdout: c.Log, Stderr: c.Err,
	}
	start := time.Now()
	code, runErr := sandbox.Run(r.ctx, spec)
	_ = listener.Close()
	served.Wait()
	failed := runErr != nil || code != 0
	r.recordLog(start, failed)
	switch {
	case result.handlerErr != nil:
		return result.handlerErr
	case runErr != nil:
		return runErr
	case code == interruptExitCode:
		return errors.New("interrupted")
	case result.reported:
		return childError(result.errData)
	case code != 0:
		return fmt.Errorf("Failure while executing; `%s` exited with %d.", strings.Join(spec.Argv(), " "), code) //nolint:staticcheck // brew's message, shown to the user
	}
	return nil
}

func childError(line string) error {
	var e struct {
		Class   string `json:"json_class"`
		Message string `json:"m"`
	}
	if json.Unmarshal([]byte(line), &e) != nil {
		return fmt.Errorf("An exception occurred within a child process:\n%s", line) //nolint:staticcheck // brew's message, shown to the user
	}
	return fmt.Errorf("An exception occurred within a child process:\n%s: %s", e.Class, e.Message) //nolint:staticcheck // brew's message, shown to the user
}

func (r *runner) recordLog(start time.Time, failed bool) {
	policy := r.c.SandboxPolicy
	if r.c.cask() {
		if failed && policy.Verbose {
			_ = sandbox.RecordLog(start, sandbox.NoLogFile, failed, policy.Verbose, r.c.Log)
		}
		return
	}
	logfile := filepath.Join(policy.Logs, r.c.Name, "postinstall.sandbox.log")
	_ = os.MkdirAll(filepath.Dir(logfile), dirMode) //fsx:allow brew's formula log folder, outside the prefix
	_ = sandbox.RecordLog(start, sandbox.LogTo(logfile), failed, policy.Verbose, r.c.Log)
}

func (r *runner) scratch(p payload, tmp string) (home, payloadPath string, cleanup func(), err error) {
	body, err := json.Marshal(p)
	if err != nil {
		return "", "", nil, err
	}
	if !r.c.cask() {
		return r.formulaScratch(body, tmp)
	}
	name, err := randomName()
	if err != nil {
		return "", "", nil, err
	}
	rel := filepath.Join(scratchRoot, "cask-"+name)
	if err := r.c.Root.MkdirAll(filepath.Join(rel, scratchHomeName), dirMode); err != nil {
		return "", "", nil, err
	}
	if err := r.c.Root.WriteFile(filepath.Join(rel, payloadName), body, fileMode); err != nil {
		return "", "", nil, err
	}
	abs := filepath.Join(r.c.Prefix, rel)
	return filepath.Join(abs, scratchHomeName), filepath.Join(abs, payloadName), func() { _ = r.c.Root.RemoveAll(rel) }, nil
}

func (r *runner) formulaScratch(body []byte, tmp string) (home, payloadPath string, cleanup func(), err error) {
	home, err = os.MkdirTemp(tmp, r.c.Name+"-postinstall-") //fsx:allow brew's Dir.mktmpdir inside the sandbox directory
	if err != nil {
		return "", "", nil, err
	}
	bazelrc := "startup --output_user_root=" + filepath.Join(home, "_bazel") + "\n"
	if err := os.WriteFile(filepath.Join(home, bazelrcName), []byte(bazelrc), fileMode); err != nil { //fsx:allow brew's setup_home inside the sandbox directory
		return "", "", nil, err
	}
	payloadPath = filepath.Join(tmp, payloadName)
	if err := os.WriteFile(payloadPath, body, fileMode); err != nil { //fsx:allow the payload inside the sandbox directory
		return "", "", nil, err
	}
	return home, payloadPath, func() {}, nil
}

func originalPaths() string {
	path, set := os.LookupEnv(homebrewPathEnv)
	if !set {
		path = os.Getenv("PATH")
	}
	var dirs []string
	for _, dir := range filepath.SplitList(path) {
		if abs, err := filepath.Abs(dir); err == nil {
			dirs = append(dirs, abs)
		}
	}
	return strings.Join(dirs, string(os.PathListSeparator))
}

func (r *runner) childEnv(home, tmp, socket string) []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !sensitiveName.MatchString(name) && name != homebrewPathEnv && name != "PATH" {
			env = append(env, kv)
		}
	}
	env = append(env, "PATH="+originalPaths(),
		"HOME="+home, "HOMEBREW_TEMP="+tmp, "TMPDIR="+tmp, "TEMP="+tmp, "TMP="+tmp,
		errorPipeEnv+"="+socket,
	)
	if r.c.cask() {
		return append(env, messageChannelEnv+"=1")
	}
	return append(env, r.commonSandboxEnv(home, tmp)...)
}

func (r *runner) packageManagerCache() map[string]string {
	dir := func(name string) string { return filepath.Join(r.c.SandboxPolicy.Cache, name) }
	return map[string]string{
		"_JAVA_OPTIONS": "-Duser.home=" + dir("java_cache"), "BUNDLE_GLOBAL_GEM_CACHE": "true", "BUNDLE_USER_CACHE": dir("bundler_cache"),
		"CABAL_DIR": dir("cabal_cache"), "CARGO_HOME": dir("cargo_cache"), "COMPOSER_CACHE_DIR": dir("composer_cache"),
		"GEM_SPEC_CACHE": dir("gem_cache"), "GOCACHE": dir("go_cache"), "GOPATH": dir("go_mod_cache"), "HEX_HOME": dir("hex_cache"),
		"NPM_CONFIG_CACHE": dir("npm_cache"), "NPM_CONFIG_STORE_DIR": dir("pnpm_cache"), "NUGET_PACKAGES": dir("nuget_cache"),
		"PIP_CACHE_DIR": dir("pip_cache"), "UV_CACHE_DIR": dir("uv_cache"), "YARN_CACHE_FOLDER": dir("yarn_cache"),
		"ZIG_GLOBAL_CACHE_DIR": dir("zig_cache"),
	}
}

func (r *runner) commonSandboxEnv(home, tmp string) []string {
	env := r.packageManagerCache()
	env["_JAVA_OPTIONS"] += " -Djava.io.tmpdir=" + tmp
	curlHome := home
	if v, set := os.LookupEnv("CURL_HOME"); set {
		curlHome = v
	}
	maps.Copy(env, map[string]string{
		"HOME": home, "TMPDIR": tmp, "TEMP": tmp, "TMP": tmp,
		"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_TERMINAL_PROMPT": "0", "GOENV": "off",
		"CARGO_REGISTRY_GLOBAL_MIN_PUBLISH_AGE": releaseCooldownAge, "BUNDLE_COOLDOWN": releaseCooldownDays,
		"PIP_CONFIG_FILE": os.DevNull, "NPM_CONFIG_USERCONFIG": os.DevNull, "CURL_HOME": curlHome,
		"PYTHONDONTWRITEBYTECODE": "1", "XDG_CONFIG_HOME": filepath.Join(home, ".config"),
	})
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

func (r *runner) serve(conn net.Conn, raw []json.RawMessage, ph phase, result *childResult) {
	parent := &runner{ctx: r.ctx, c: r.c, guards: map[string]bool{}}
	parent.c.Sandbox = Unsandboxed
	if _, err := fmt.Fprintln(conn, parentGreeting); err != nil {
		return
	}
	var order requestOrder
	sc := bufio.NewScanner(conn)
	sc.Buffer(nil, maxMessageBytes)
	for sc.Scan() {
		line := sc.Text()
		var msg map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &msg) == nil {
			if _, isError := msg["json_class"]; isError {
				result.setError(line)
				continue
			}
		}
		index, err := parseRequest(line, order)
		if err == nil {
			err = parent.stepAt(ph, raw, index)
		}
		if err != nil {
			result.setHandler(err)
			return
		}
		order = order.after(index)
		if _, err := fmt.Fprintln(conn, privilegedSucceed); err != nil {
			return
		}
	}
}

// requestOrder remembers the last privileged step run; before the first request there is none.
type requestOrder struct {
	last      int
	requested bool
}

func (o requestOrder) permits(index int) bool { return !o.requested || index > o.last }

func (o requestOrder) after(index int) requestOrder {
	return requestOrder{last: index, requested: true}
}

func parseRequest(line string, order requestOrder) (int, error) {
	var req struct {
		Type  string          `json:"type"`
		Index json.RawMessage `json:"index"`
	}
	if err := json.Unmarshal([]byte(line), &req); err != nil || req.Type != privilegedRequest {
		return 0, errors.New("Invalid privileged cask child message.") //nolint:staticcheck // brew's message, shown to the user
	}
	var index int
	if err := json.Unmarshal(req.Index, &index); err != nil {
		return 0, fmt.Errorf("Invalid privileged cask install step index: %s", req.Index) //nolint:staticcheck // brew's message, shown to the user
	}
	if !order.permits(index) {
		return 0, errors.New("Privileged cask install steps must be requested in order.") //nolint:staticcheck // brew's message, shown to the user
	}
	return index, nil
}

func (r *runner) homeWritePaths(home string) []string {
	var paths []string
	for _, rel := range []string{"Library/Developer", "Library/Caches/org.swift.swiftpm"} {
		paths = append(paths, filepath.Join(home, rel))
	}
	return paths
}

// Rules are added in brew's order: of the rules with a path filter that match, seatbelt applies the last.
func (r *runner) profile(raw []json.RawMessage, ph phase, home, payloadPath, tmp string) (*sandbox.Sandbox, error) {
	c, policy := r.c, r.c.SandboxPolicy
	sb := &sandbox.Sandbox{}
	realHome, err := r.base("home", "")
	if err != nil {
		return nil, err
	}
	network := policy.NetworkAccess
	if c.cask() {
		sb.AllowRead(c.Staged)
		network = slices.ContainsFunc(raw, func(b json.RawMessage) bool {
			var s step
			return json.Unmarshal(b, &s) == nil && s.Type == "run" && s.NetworkAccess
		})
	} else {
		sb.AllowWritePath(filepath.Join(policy.Logs, c.Name))
		for _, p := range r.homeWritePaths(realHome) {
			sb.AllowWritePath(p)
		}
		sb.AllowWriteRegex(`^/private/var/folders/[^/]+/[^/]+/T/xcrun_db(-[^/]+)?$`)
		for _, p := range []string{filepath.Join(c.Prefix, "Cellar", c.Name), filepath.Join(c.Prefix, "etc"), filepath.Join(c.Prefix, "var")} {
			sb.AllowWritePath(p)
		}
	}
	sb.AllowWriteSystemTemp(c.Temp)
	sb.AllowWritePath(policy.Cache)
	sb.DenyWriteHomebrewRepository(policy.BrewFile, c.Prefix, policy.Repository)
	readable := []string{c.Prefix, policy.Repository, policy.Cache, policy.Logs, c.Temp, trust.Path()}
	for _, name := range ciPathVariables {
		if p, set := os.LookupEnv(name); set && p != "" {
			readable = append(readable, p)
		}
	}
	for _, p := range r.homeWritePaths(realHome) {
		if _, err := os.Stat(p); err == nil {
			readable = append(readable, p)
		}
	}
	sb.DenyReadHome(realHome, readable, func(m string) { r.logf("Warning: %s", m) })
	if !network {
		sb.DenyAllNetwork()
	}
	if c.cask() {
		sb.AllowWritePath(filepath.Join(c.Prefix, "Caskroom", c.Token))
		sb.AllowWritePath(c.AppDir)
	}
	for _, d := range kegLinkDirectories {
		sb.AllowWritePath(filepath.Join(c.Prefix, d))
	}
	if c.cask() {
		paths, err := r.writePaths(raw, ph)
		if err != nil {
			return nil, err
		}
		for _, p := range paths {
			sb.AllowWritePath(p)
			if sandbox.Within(realHome, p) {
				sb.AllowRead(p)
			}
		}
		sb.AllowWritePath(home)
		sb.AllowReadLiteral(payloadPath)
	} else if libexec := filepath.Join(c.Prefix, "libexec"); isDir(libexec) {
		sb.AllowWritePath(libexec)
	}
	sb.DenyWritePath(filepath.Join(c.Prefix, tempCellar))
	locks := filepath.Join(c.Prefix, locksDir)
	sb.DenyWritePath(locks)
	sb.DenyReadPath(locks)
	sb.AllowRead(tmp)
	sb.AllowWritePath(tmp)
	sb.AllowNetworkSubpath(tmp)
	return sb, nil
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (r *runner) writePaths(raw []json.RawMessage, ph phase) ([]string, error) {
	var out []string
	add := func(spec *pathSpec, parent bool) error {
		if spec == nil {
			return nil
		}
		p, err := r.resolve(*spec)
		if err != nil {
			return err
		}
		if parent {
			p = filepath.Dir(p)
		}
		out = append(out, p)
		return nil
	}
	for i, b := range raw {
		s, err := r.parse(ph, i, b)
		if err != nil {
			return nil, err
		}
		if ph == phaseUninstall {
			if s.Type == "symlink" && s.Uninstall {
				if err := add(s.Target, true); err != nil {
					return nil, err
				}
			}
			continue
		}
		switch s.Type {
		case "mkdir_p":
			p, err := r.target(s.Path)
			if err != nil {
				return nil, err
			}
			if !isDir(filepath.Dir(p)) {
				out = append(out, filepath.Dir(p))
			} else {
				out = append(out, p)
			}
		case "mkdir", "touch", "write":
			err = add(s.Path, true)
		case "move":
			if err = add(s.Source, true); err == nil {
				err = add(s.Target, true)
			}
		case "move_children", "move_contents":
			if err = add(s.Source, false); err == nil {
				err = add(s.Target, false)
			}
		case "copy", "symlink":
			err = add(s.Target, true)
		case "remove":
			for _, spec := range s.Paths {
				matches, gerr := r.glob(spec)
				if gerr != nil {
					return nil, gerr
				}
				for _, m := range matches {
					out = append(out, filepath.Dir(m))
				}
			}
		case "inreplace", "change_dylib_id":
			spec := s.Path
			if s.Type == "change_dylib_id" {
				spec = s.Source
			}
			var p string
			if spec == nil {
				return nil, errors.New("the step has no path")
			}
			if p, err = r.resolve(*spec); err == nil {
				if _, serr := os.Stat(p); serr != nil {
					p = filepath.Dir(p)
				}
				out = append(out, p)
			}
		case "link_dir", "link_children":
			err = add(s.Target, false)
		case "run":
			if err = add(s.StdoutPath, true); err == nil {
				for _, spec := range s.WritablePaths {
					if err = add(&spec, false); err != nil {
						break
					}
				}
			}
		case "set_permissions", "set_ownership":
			var paths []string
			if paths, err = r.existingPaths(s); err == nil {
				out = append(out, paths...)
			}
		}
		if err != nil {
			return nil, err
		}
	}
	return slices.Compact(out), nil
}
