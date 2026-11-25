package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/jigsmiths/brw/internal/services"
)

const (
	launchctlPath = "/bin/launchctl"
	pbpastePath   = "/usr/bin/pbpaste"
	ansiMagenta   = "\x1b[35m"
	ttyMargin     = 4
	defaultWidth  = 80
)

type debugLaunchctl struct {
	services.Launchctl
	e *env
}

func (d debugLaunchctl) Print(domain, label string) (string, error) {
	out, err := d.Launchctl.Print(domain, label)
	if err == nil && strings.TrimSpace(out) != "" {
		d.e.odebug(launchctlPath+" print "+domain+"/"+label, out)
	}
	return out, err
}

func (d debugLaunchctl) List(label string) (string, error) {
	out, err := d.Launchctl.List(label)
	d.e.odebug(launchctlPath+" list "+label, out)
	return out, err
}

func (e *env) odebug(title, body string) {
	if e.tty {
		_, _ = fmt.Fprintf(e.err, "%s==>%s %s%s%s\n", ansiMagenta, ansiReset, ansiBold, title, ansiReset)
	} else {
		_, _ = fmt.Fprintf(e.err, "==> %s\n", title)
	}
	_, _ = fmt.Fprint(e.err, body)
	if !strings.HasSuffix(body, "\n") {
		_, _ = fmt.Fprintln(e.err)
	}
}

// echo is what SystemCommand.safe_system prints under --verbose.
func (e *env) echo(args ...string) {
	if e.verbose {
		_, _ = fmt.Fprintln(e.out, launchctlPath+" "+strings.Join(args, " "))
	}
}

// echoQuiet is what SystemCommand.run prints to stderr under --verbose --debug.
func (e *env) echoQuiet(args ...string) {
	if !e.verbose || !e.debug {
		return
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = strings.ReplaceAll(shQuote(a), `\=`, "=")
	}
	_, _ = fmt.Fprintln(e.err, launchctlPath+" "+strings.Join(quoted, " "))
}

func (e *env) truncate(title string) string {
	if !e.tty || e.verbose || e.termWidth == nil {
		return title
	}
	width := e.termWidth()
	if width == 0 {
		return title
	}
	runes := []rune(title)
	if keep := width - ttyMargin; keep < len(runes) {
		return string(runes[:max(keep, 0)])
	}
	return title
}

func stdinWidth() int {
	if w := terminalWidth(os.Stdin); w > 0 {
		return w
	}
	return defaultWidth
}

func pbpasteBroken() bool {
	if fi, err := os.Stat(pbpastePath); err != nil || fi.Mode()&0o111 == 0 {
		return false
	}
	return exec.Command(pbpastePath).Run() != nil
}
