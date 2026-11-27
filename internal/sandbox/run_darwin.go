package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"unsafe"
)

const (
	ptmxPath     = "/dev/ptmx"
	ptsNameBytes = 128
	childTTYFD   = 0

	// sys/ioccom.h: the direction bits and the shifts _IOC packs a request from.
	iocVoid     = 0x20000000
	iocOut      = 0x40000000
	iocIn       = 0x80000000
	iocLenShift = 16
	iocGroup    = 't' << 8
	winsizeLen  = 8 // sizeof(struct winsize)

	// sys/ttycom.h: _IO('t', 84), _IO('t', 82), _IOC(IOC_OUT, 't', 83, 128), _IOR('t', 104, struct winsize), _IOW('t', 103, struct winsize).
	tiocPtyGrant  = iocVoid | iocGroup | 84
	tiocPtyUnlock = iocVoid | iocGroup | 82
	tiocPtyGName  = iocOut | ptsNameBytes<<iocLenShift | iocGroup | 83
	tiocGWinSz    = iocOut | winsizeLen<<iocLenShift | iocGroup | 104
	tiocSWinSz    = iocIn | winsizeLen<<iocLenShift | iocGroup | 103

	sttyCommand = "stty"
	tputCommand = "tput"
)

type winsize struct{ rows, cols, xpixel, ypixel uint16 }

func ioctl(fd uintptr, request uintptr, arg unsafe.Pointer) error {
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(arg)); errno != 0 {
		return errno
	}
	return nil
}

func openPTY() (controller, worker *os.File, err error) {
	controller, err = os.OpenFile(ptmxPath, os.O_RDWR|syscall.O_NOCTTY, 0) //fsx:allow the pseudo-terminal master; nothing on disk is written
	if err != nil {
		return nil, nil, err
	}
	fd := controller.Fd()
	if err = ioctl(fd, tiocPtyGrant, nil); err == nil {
		err = ioctl(fd, tiocPtyUnlock, nil)
	}
	var name [ptsNameBytes]byte
	if err == nil {
		err = ioctl(fd, tiocPtyGName, unsafe.Pointer(&name[0]))
	}
	if err == nil {
		path := string(name[:strings.IndexByte(string(name[:]), 0)])
		worker, err = os.OpenFile(path, os.O_RDWR|syscall.O_NOCTTY, 0) //fsx:allow the pseudo-terminal slave; nothing on disk is written
	}
	if err != nil {
		_ = controller.Close()
		return nil, nil, err
	}
	return controller, worker, nil
}

func windowSize(f *os.File) (winsize, error) {
	var ws winsize
	return ws, ioctl(f.Fd(), tiocGWinSz, unsafe.Pointer(&ws))
}

var tputNumber = func(capability string) (uint16, bool) {
	out, err := exec.Command(tputCommand, capability).Output()
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 16)
	return uint16(n), err == nil
}

func sizeFromTput() (winsize, bool) {
	rows, rowsKnown := tputNumber("lines")
	cols, colsKnown := tputNumber("cols")
	if !rowsKnown || !colsKnown {
		return winsize{}, false
	}
	return winsize{rows: rows, cols: cols}, true
}

func terminalSize() (ws winsize, known bool) {
	if ws, err := windowSize(os.Stdout); err == nil {
		return ws, true
	}
	return sizeFromTput()
}

func isTerminal(f *os.File) bool {
	_, err := windowSize(f)
	return err == nil
}

func watchWindowSize(controller *os.File) (stop func()) {
	resize := func() {
		if ws, known := terminalSize(); known {
			_ = ioctl(controller.Fd(), tiocSWinSz, unsafe.Pointer(&ws))
		}
	}
	resize()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	finished := make(chan struct{})
	go func() {
		for {
			select {
			case <-winch:
				resize()
			case <-finished:
				return
			}
		}
	}()
	return func() { signal.Stop(winch); close(finished) }
}

func rawStdin(s Spec) (restore func()) {
	in, isFile := s.Stdin.(*os.File)
	if !s.PassStdin || !isFile || !isTerminal(in) {
		return func() {}
	}
	stty := func(args ...string) ([]byte, error) {
		cmd := exec.Command(sttyCommand, args...)
		cmd.Stdin = in
		return cmd.Output()
	}
	saved, err := stty("-g")
	if err != nil {
		return func() {}
	}
	if _, err := stty("raw", "-echo", "opost"); err != nil {
		return func() {}
	}
	return func() { _, _ = stty(strings.TrimSpace(string(saved))) }
}

// execute runs cmd on a pseudo-terminal of its own, so it cannot reach the parent's controlling terminal.
func execute(cmd *exec.Cmd, s Spec) error {
	controller, worker, err := openPTY()
	if err != nil {
		return err
	}
	defer func() { _ = controller.Close() }()
	stopResize := watchWindowSize(controller)
	defer stopResize()
	signal.Ignore(syscall.SIGTTIN, syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTIN, syscall.SIGTTOU)
	defer rawStdin(s)()

	cmd.Stdin, cmd.Stdout, cmd.Stderr = worker, worker, worker
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: childTTYFD}
	err = cmd.Start()
	_ = worker.Close()
	if err != nil {
		return err
	}
	done := forward(controller, s.Stdin, s.Stdout, s.PassStdin)
	err = cmd.Wait()
	<-done
	return errors.Join(err)
}
