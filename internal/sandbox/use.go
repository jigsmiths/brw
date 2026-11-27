package sandbox

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const InheritanceFD = 198

const (
	sandboxProbeProfile = "(version 1)(allow default)"
	probeCommand        = "/usr/bin/true"
	syslogCommand       = "syslog"
	logSettleDelay      = 100 * time.Millisecond
	logFooter           = "\nWe use time to filter sandbox log. Therefore, unrelated logs may be recorded.\n"
	logFileMode         = 0o644
	accessWrite         = 0x2
)

type Decision int

const (
	Invalid Decision = iota
	Use
	Skip
)

type PrivilegedGroup struct {
	Name   string
	Member bool
}

type Options struct {
	Inherited       bool
	Available       bool
	Nested          bool
	AvoidNested     bool
	DefaultPrefix   bool
	Prefix          string
	PrivilegedGroup PrivilegedGroup
}

type LogDestination struct {
	path string
	kept bool
}

var NoLogFile = LogDestination{}

func LogTo(path string) LogDestination { return LogDestination{path: path, kept: true} }

func Decide(step string, o Options) (Decision, string, error) {
	if o.Inherited {
		return Skip, "", nil
	}
	if !o.Available {
		return Skip, fmt.Sprintf("Sandbox unavailable: %s without sandboxing!", step), nil
	}
	if !o.AvoidNested || !o.Nested {
		return Use, "", nil
	}
	if o.DefaultPrefix {
		return Invalid, "", fmt.Errorf("Refusing to skip the sandbox: `$HOMEBREW_AVOID_NESTED_SANDBOXING` is set inside another sandbox but brw is using its default prefix (%s); this is only supported in a custom prefix.", o.Prefix) //nolint:staticcheck // brew's message, shown to the user
	}
	if o.PrivilegedGroup.Member {
		return Invalid, "", fmt.Errorf("Refusing to skip the sandbox: `$HOMEBREW_AVOID_NESTED_SANDBOXING` is set inside another sandbox but you are in the privileged `%s` group; this is only supported for an unprivileged user.", o.PrivilegedGroup.Name) //nolint:staticcheck // brew's message, shown to the user
	}
	return Skip, fmt.Sprintf("%s without brw's sandbox; relying on the outer sandbox.", upperFirst(step)), nil
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

var (
	nestedOnce   sync.Once
	nestedResult bool
)

// Nested reports whether this process is already sandboxed; a sandbox cannot be applied inside another.
func Nested() bool {
	nestedOnce.Do(func() {
		nestedResult = exec.Command(Exec, "-p", sandboxProbeProfile, probeCommand).Run() != nil
	})
	return nestedResult
}

func Inherited(exe, brewFile string) (bool, error) {
	var held, want syscall.Stat_t
	if err := syscall.Fstat(InheritanceFD, &held); err != nil {
		return false, nil
	}
	if err := syscall.Stat(exe, &want); err != nil || held.Dev != want.Dev || held.Ino != want.Ino {
		return false, nil
	}
	if syscall.Access(brewFile, accessWrite) != nil {
		return true, nil
	}
	f, err := os.OpenFile(brewFile, os.O_WRONLY, 0) //fsx:allow brew's probe of whether the sandbox protects its own file; nothing is written
	switch {
	case err == nil:
		_ = f.Close()
		return false, fmt.Errorf("Inherited sandbox permits writes to %s", brewFile) //nolint:staticcheck // brew's message, shown to the user
	case errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EROFS):
		return true, nil
	}
	return false, err
}

func (s *Sandbox) AllowNetworkSubpath(path string) { s.add(true, "network*", subpath, path) }

var bytecodeWrite = regexp.MustCompile(`Python\(\d+\) deny file-write.*pyc$`)

func filterLog(logs string) string {
	var keep strings.Builder
	for _, line := range strings.SplitAfter(logs, "\n") {
		if !bytecodeWrite.MatchString(strings.TrimSuffix(line, "\n")) {
			keep.WriteString(line)
		}
	}
	return keep.String()
}

var syslogQuery = func(args ...string) string {
	out, _ := exec.Command(syslogCommand, args...).Output()
	return string(out)
}

func RecordLog(start time.Time, dest LogDestination, failed, verbose bool, display io.Writer) error {
	time.Sleep(logSettleDelay)
	since := fmt.Sprint(start.Unix())
	out := syslogQuery(
		"-F", "$((Time)(local)) $(Sender)[$(PID)]: $(Message)",
		"-k", "Time", "ge", since, "-k", "Message", "S", "deny", "-k", "Sender", "kernel",
		"-o",
		"-k", "Time", "ge", since, "-k", "Message", "S", "deny", "-k", "Sender", "sandboxd")
	logs := filterLog(out)
	if logs == "" {
		return nil
	}
	if dest.kept {
		if err := os.WriteFile(dest.path, []byte(logs+logFooter), logFileMode); err != nil { //fsx:allow brew's sandbox log, outside the prefix
			return err
		}
	}
	if failed && verbose {
		_, err := fmt.Fprintf(display, "==> Sandbox Log\n%s", logs)
		return err
	}
	return nil
}
