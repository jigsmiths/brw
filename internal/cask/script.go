package cask

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

const (
	envCommand  = "/usr/bin/env"
	sudoCommand = "/usr/bin/sudo"
	redacted    = "******"
	ansiRed     = "\x1b[31m"
	ansiReset   = "\x1b[0m"
)

var sudoBinary = sudoCommand

var sensitiveName = regexp.MustCompile(`(?i)(cookie|key|token|password|passphrase|auth)`)

var falsyValues = []string{"false", "no", "off", "nil", "0"}

var signalNames = map[syscall.Signal]string{
	syscall.SIGHUP: "HUP", syscall.SIGINT: "INT", syscall.SIGQUIT: "QUIT", syscall.SIGILL: "ILL",
	syscall.SIGTRAP: "TRAP", syscall.SIGABRT: "ABRT", syscall.SIGFPE: "FPE", syscall.SIGKILL: "KILL",
	syscall.SIGBUS: "BUS", syscall.SIGSEGV: "SEGV", syscall.SIGSYS: "SYS", syscall.SIGPIPE: "PIPE",
	syscall.SIGALRM: "ALRM", syscall.SIGTERM: "TERM", syscall.SIGURG: "URG", syscall.SIGSTOP: "STOP",
	syscall.SIGTSTP: "TSTP", syscall.SIGCONT: "CONT", syscall.SIGCHLD: "CHLD", syscall.SIGTTIN: "TTIN",
	syscall.SIGTTOU: "TTOU", syscall.SIGIO: "IO", syscall.SIGXCPU: "XCPU", syscall.SIGXFSZ: "XFSZ",
	syscall.SIGVTALRM: "VTALRM", syscall.SIGPROF: "PROF", syscall.SIGWINCH: "WINCH",
	syscall.SIGUSR1: "USR1", syscall.SIGUSR2: "USR2",
}

func signalName(s syscall.Signal) string {
	if name, ok := signalNames[s]; ok {
		return name
	}
	return platformSignalNames[s]
}

func envEnabled(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return v != "" && !slices.Contains(falsyValues, strings.ToLower(v))
}

func colorEnabled(w io.Writer) bool {
	if strings.TrimSpace(os.Getenv("HOMEBREW_NO_COLOR")) != "" || strings.TrimSpace(os.Getenv("NO_COLOR")) != "" {
		return false
	}
	if strings.TrimSpace(os.Getenv("HOMEBREW_COLOR")) != "" {
		return true
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// columns is Tty.width: the terminal on stdin, else tput, else 80.
func columns(size func() (int, bool), tput func() string) int {
	if w, ok := size(); ok {
		return w
	}
	out := strings.TrimSpace(tput())
	if out == "" {
		return 80
	}
	n, _ := strconv.Atoi(out)
	return n
}

func stdinColumns() (int, bool) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	return int(ws.Col), errno == 0
}

func tputColumns() string {
	out, _ := exec.Command("/usr/bin/tput", "cols").Output()
	return string(out)
}

func sensitiveValues() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if sensitiveName.MatchString(name) && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func redact(s string, secrets []string) string {
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

func keyOrder(raw json.RawMessage) ([]string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	if _, err := d.Token(); err != nil {
		return nil, err
	}
	var keys []string
	for d.More() {
		k, err := d.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, k.(string))
		var skip json.RawMessage
		if err := d.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

var plainSymbol = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*[?!=]?$`)

func rubySymbols(names []string) string {
	parts := make([]string, len(names))
	for i, n := range names {
		if plainSymbol.MatchString(n) {
			parts[i] = ":" + n
		} else {
			parts[i] = ":" + fmt.Sprintf("%q", n)
		}
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

func scriptCommand(r ScriptRun) []string {
	if !r.Sudo {
		return append([]string{envCommand, r.Executable}, r.Args...)
	}
	var askpass []string
	if r.Askpass {
		askpass = []string{"-A"}
	}
	envArgs := []string{"-E", "PATH=" + shellEscape(r.Path), "--"}
	cmd := []string{sudoCommand}
	if r.SudoUser != "" {
		cmd = append(cmd, "--prompt", "Password for %p:", "-u", r.SudoUser)
		cmd = append(cmd, askpass...)
		cmd = append(cmd, envArgs...)
		cmd = append(cmd, sudoCommand)
	}
	cmd = append(cmd, askpass...)
	cmd = append(cmd, envArgs...)
	cmd = append(cmd, r.Executable)
	return append(cmd, r.Args...)
}

func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = shellEscape(a)
	}
	return strings.ReplaceAll(strings.Join(parts, " "), `\=`, "=")
}

func failureMessage(argv []string, reason, output string, secrets []string) string {
	msg := fmt.Sprintf("Failure while executing; `%s` %s.", redact(shellJoin(argv), secrets), reason)
	if output == "" {
		return msg
	}
	msg += " Here's the output:\n" + output
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	return msg
}

type logLine struct {
	text   string
	stderr bool
}

type outputLog struct {
	mu    sync.Mutex
	lines []logLine
}

func (l *outputLog) add(line logLine) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, line)
}

func (l *outputLog) render(color bool) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, line := range l.lines {
		if line.stderr && color {
			b.WriteString(ansiRed + line.text + ansiReset)
		} else {
			b.WriteString(line.text)
		}
	}
	return b.String()
}

type lineSink struct {
	secrets []string
	log     *outputLog
	out     io.Writer
	stderr  bool
	partial []byte
}

func newLineSink(secrets []string, log *outputLog, out io.Writer, stderr bool) *lineSink {
	return &lineSink{secrets: secrets, log: log, out: out, stderr: stderr}
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.partial = append(s.partial, p...)
	for {
		i := bytes.IndexByte(s.partial, '\n')
		if i < 0 {
			return len(p), nil
		}
		s.emit(string(s.partial[:i+1]))
		s.partial = s.partial[i+1:]
	}
}

func (s *lineSink) Flush() {
	if len(s.partial) > 0 {
		s.emit(string(s.partial))
		s.partial = nil
	}
}

func (s *lineSink) emit(line string) {
	line = redact(line, s.secrets)
	s.log.add(logLine{text: line, stderr: s.stderr})
	if s.out != nil {
		_, _ = io.WriteString(s.out, line)
	}
}

func (in *Installer) sudoAvailable(ctx context.Context) bool {
	if strings.TrimSpace(os.Getenv("HOMEBREW_NO_SUDO")) != "" {
		return false
	}
	if os.Getenv("HOMEBREW_SUDO_CHECKED") == "1" {
		return true
	}
	if !in.sudoUsable(ctx) {
		_ = os.Setenv("HOMEBREW_NO_SUDO", "1")
	}
	_ = os.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	return strings.TrimSpace(os.Getenv("HOMEBREW_NO_SUDO")) == ""
}

var sudoRefusals = []string{`The "no new privileges" flag is set`, "effective uid is not 0", "must be owned by uid 0 and have the setuid bit set"}

var sudoDenials = []string{" is not in the sudoers file.", " is not allowed to run sudo on ", " may not run sudo on "}

func (in *Installer) sudoUsable(ctx context.Context) bool {
	sudo := sudoBinary
	if fi, err := os.Stat(sudo); err != nil || fi.Mode()&0o111 == 0 {
		var err error
		if sudo, err = exec.LookPath("sudo"); err != nil {
			return false
		}
	}
	mentions := func(text string, phrases ...[]string) bool {
		for _, list := range phrases {
			for _, p := range list {
				if strings.Contains(text, p) {
					return true
				}
			}
		}
		return false
	}
	run := func(args ...string) (string, bool) {
		out, err := in.exec(ctx, envCommand, append([]string{"LC_ALL=C", sudo}, args...)...)
		if err != nil {
			return string(out) + err.Error(), false
		}
		return string(out), true
	}
	if text, ok := run("--reset-timestamp"); !ok && mentions(text, sudoRefusals) {
		return false
	}
	text, ok := run("-n", "-k", "-l")
	return ok || !mentions(text, sudoRefusals, sudoDenials)
}

func execScript(ctx context.Context, r ScriptRun) error {
	argv := scriptCommand(r)
	if r.Trace != nil {
		_, _ = fmt.Fprintln(r.Trace, redact(shellJoin(argv), r.Secrets))
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "PATH="+r.Path, "COLUMNS="+strconv.Itoa(columns(stdinColumns, tputColumns)))
	if r.Input != "" {
		cmd.Stdin = strings.NewReader(r.Input)
	}
	var log outputLog
	stdout, stderr := newLineSink(r.Secrets, &log, r.Stdout, false), newLineSink(r.Secrets, &log, r.Stderr, true)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	stdout.Flush()
	stderr.Flush()
	exitErr, ok := errors.AsType[*exec.ExitError](err)
	if !ok {
		return err
	}
	if !r.MustSucceed {
		return nil
	}
	reason := fmt.Sprintf("exited with %d", exitErr.ExitCode())
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		reason = "was terminated by uncaught signal " + signalName(ws.Signal())
	}
	return brewMessage(failureMessage(argv, reason, log.render(r.Color), r.Secrets))
}
