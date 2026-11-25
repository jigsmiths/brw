package sudo

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"unsafe"
)

const (
	redacted  = "******"
	ansiRed   = "\x1b[31m"
	ansiReset = "\x1b[0m"
)

var (
	binary = sudoCommand

	sensitiveName = regexp.MustCompile(`(?i)(cookie|key|token|password|passphrase|auth)`)
	falsyValues   = []string{"false", "no", "off", "nil", "0"}

	sudoRefusals = []string{`The "no new privileges" flag is set`, "effective uid is not 0", "must be owned by uid 0 and have the setuid bit set"}
	sudoDenials  = []string{" is not in the sudoers file.", " is not allowed to run sudo on ", " may not run sudo on "}
)

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

var forbiddenInTests = []string{"/usr/bin/sudo", "/usr/sbin/installer", "/usr/sbin/pkgutil", "/bin/launchctl", "/usr/bin/trash"}

// Guard panics when a test binary is about to run a command that changes the machine.
func Guard(exe string) {
	if testing.Testing() && slices.Contains(forbiddenInTests, exe) {
		panic("a test ran " + exe + " for real; stub the runner")
	}
}

// ParseSignal is Process.kill's lookup of a signal name, with or without its SIG prefix.
func ParseSignal(name string) (syscall.Signal, error) {
	bare := strings.TrimPrefix(name, "SIG")
	if bare == "EXIT" {
		return 0, nil
	}
	if sig, ok := signalAliases[bare]; ok {
		return sig, nil
	}
	for _, names := range []map[syscall.Signal]string{signalNames, platformSignalNames} {
		for sig, n := range names {
			if n == bare {
				return sig, nil
			}
		}
	}
	return 0, message("unsupported signal 'SIG" + bare + "'")
}

func EnvSet(name string) bool { return strings.TrimSpace(os.Getenv(name)) != "" }

func EnvTrue(name string) bool {
	return EnvSet(name) && !slices.Contains(falsyValues, strings.ToLower(strings.TrimSpace(os.Getenv(name))))
}

// ColorEnabled is Tty#color? for output written to w.
func ColorEnabled(w io.Writer) bool {
	if EnvSet("HOMEBREW_NO_COLOR") || EnvSet("NO_COLOR") {
		return false
	}
	if EnvSet("HOMEBREW_COLOR") {
		return true
	}
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

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

func TerminalColumns() int { return columns(stdinColumns, tputColumns) }

func stdinColumns() (int, bool) {
	var ws struct{ Row, Col, X, Y uint16 }
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, os.Stdin.Fd(), syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	return int(ws.Col), errno == 0
}

func tputColumns() string {
	out, _ := exec.Command("/usr/bin/tput", "cols").Output()
	return string(out)
}

func SensitiveValues() []string {
	var out []string
	for _, kv := range os.Environ() {
		name, value, _ := strings.Cut(kv, "=")
		if sensitiveName.MatchString(name) && !slices.Contains(out, value) {
			out = append(out, value)
		}
	}
	return out
}

func Redact(s string, secrets []string) string {
	for _, secret := range secrets {
		s = strings.ReplaceAll(s, secret, redacted)
	}
	return s
}

// ShellEscape is Ruby's String#shellescape.
func ShellEscape(s string) string {
	if s == "" {
		return "''"
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n':
			b.WriteString("'\n'")
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("_-.,:+/@", r):
			b.WriteRune(r)
		default:
			b.WriteByte('\\')
			b.WriteRune(r)
		}
	}
	return b.String()
}

func ShellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		parts[i] = ShellEscape(a)
	}
	return strings.ReplaceAll(strings.Join(parts, " "), `\=`, "=")
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

func (l *outputLog) stream(stderr bool) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var b strings.Builder
	for _, line := range l.lines {
		if line.stderr == stderr {
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
	line = Redact(line, s.secrets)
	s.log.add(logLine{text: line, stderr: s.stderr})
	if s.out != nil {
		_, _ = io.WriteString(s.out, line)
	}
}

// Available is utils/sudo.sh. It records its verdict in HOMEBREW_NO_SUDO and HOMEBREW_SUDO_CHECKED as brew does.
func Available(ctx context.Context, run Runner) bool {
	if EnvSet("HOMEBREW_NO_SUDO") {
		return false
	}
	if os.Getenv("HOMEBREW_SUDO_CHECKED") == "1" {
		return true
	}
	if !usable(ctx, run) {
		_ = os.Setenv("HOMEBREW_NO_SUDO", "1")
	}
	_ = os.Setenv("HOMEBREW_SUDO_CHECKED", "1")
	return !EnvSet("HOMEBREW_NO_SUDO")
}

func usable(ctx context.Context, run Runner) bool {
	sudo := binary
	if fi, err := os.Stat(sudo); err != nil || fi.Mode()&0o111 == 0 {
		var err error
		if sudo, err = exec.LookPath("sudo"); err != nil {
			return false
		}
	}
	probe := func(args ...string) (string, bool) {
		res, err := run(ctx, Command{Exe: sudo, Args: args, Env: []string{"LC_ALL=C"}, MustSucceed: true})
		if err != nil {
			return res.Stdout + res.Stderr + err.Error(), false
		}
		return "", true
	}
	mentions := func(text string, lists ...[]string) bool {
		for _, list := range lists {
			if slices.ContainsFunc(list, func(p string) bool { return strings.Contains(text, p) }) {
				return true
			}
		}
		return false
	}
	if text, ok := probe("--reset-timestamp"); !ok && mentions(text, sudoRefusals) {
		return false
	}
	text, ok := probe("-n", "-k", "-l")
	return ok || !mentions(text, sudoRefusals, sudoDenials)
}
