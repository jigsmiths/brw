package sandbox

import (
	"runtime"
	"syscall"
	"unsafe"
)

// libSystem is reached the way x/sys/unix and the standard library's syscall package reach it on macOS, without cgo
// or another module: dynamic imports, assembly trampolines for their addresses, and the runtime's syscall entry points.
// The private responsibility_spawnattrs_setdisclaim is looked up with dlsym when needed, as brew's Fiddle code does.

//go:cgo_import_dynamic libc_posix_spawn posix_spawn "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_init posix_spawnattr_init "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_setflags posix_spawnattr_setflags "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_posix_spawnattr_destroy posix_spawnattr_destroy "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic libc_dlsym dlsym "/usr/lib/libSystem.B.dylib"
//go:cgo_import_dynamic _ _ "/usr/lib/libSystem.B.dylib"

//go:linkname syscall_syscall syscall.syscall
func syscall_syscall(fn, a1, a2, a3 uintptr) (r1, r2 uintptr, err syscall.Errno)

//go:linkname syscall_syscall6 syscall.syscall6
func syscall_syscall6(fn, a1, a2, a3, a4, a5, a6 uintptr) (r1, r2 uintptr, err syscall.Errno)

var (
	libc_posix_spawn_trampoline_addr              uintptr
	libc_posix_spawnattr_init_trampoline_addr     uintptr
	libc_posix_spawnattr_setflags_trampoline_addr uintptr
	libc_posix_spawnattr_destroy_trampoline_addr  uintptr
	libc_dlsym_trampoline_addr                    uintptr
)

const (
	// POSIX_SPAWN_SETEXEC in sys/spawn.h.
	posixSpawnSetExec = 0x0040
	// RTLD_DEFAULT in dlfcn.h is ((void *) -2): search every image in the process.
	rtldDefault            = ^uintptr(1)
	disclaimResponsibility = 1

	disclaimSymbol = "responsibility_spawnattrs_setdisclaim"
)

var resolveDisclaim = func() (fn uintptr, found bool) {
	name, err := syscall.BytePtrFromString(disclaimSymbol)
	if err != nil {
		return 0, false
	}
	addr, _, _ := syscall_syscall(libc_dlsym_trampoline_addr, rtldDefault, uintptr(unsafe.Pointer(name)), 0)
	runtime.KeepAlive(name)
	return addr, addr != 0
}

func cstrings(items []string) ([]*byte, error) {
	out := make([]*byte, 0, len(items)+1)
	for _, s := range items {
		p, err := syscall.BytePtrFromString(s)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return append(out, nil), nil
}

// ExecDisclaimed is Utils::Disclaim.exec: it replaces this process with argv, run so that macOS does not hold
// this process's parent (the terminal app) responsible for what argv asks to read. It returns only on failure.
func ExecDisclaimed(argv, env []string) error {
	disclaim, found := resolveDisclaim()
	if !found {
		return ErrDisclaimUnavailable
	}
	path, err := syscall.BytePtrFromString(argv[0])
	if err != nil {
		return err
	}
	cargv, err := cstrings(argv)
	if err != nil {
		return err
	}
	cenv, err := cstrings(env)
	if err != nil {
		return err
	}
	var attr uintptr
	if _, _, errno := syscall_syscall(libc_posix_spawnattr_init_trampoline_addr, uintptr(unsafe.Pointer(&attr)), 0, 0); errno != 0 {
		return errno
	}
	defer func() {
		_, _, _ = syscall_syscall(libc_posix_spawnattr_destroy_trampoline_addr, uintptr(unsafe.Pointer(&attr)), 0, 0)
	}()
	if _, _, errno := syscall_syscall(libc_posix_spawnattr_setflags_trampoline_addr, uintptr(unsafe.Pointer(&attr)), posixSpawnSetExec, 0); errno != 0 {
		return errno
	}
	if r, _, _ := syscall_syscall(disclaim, uintptr(unsafe.Pointer(&attr)), disclaimResponsibility, 0); r != 0 {
		return ErrDisclaimUnavailable
	}
	r, _, _ := syscall_syscall6(libc_posix_spawn_trampoline_addr, 0, uintptr(unsafe.Pointer(path)),
		0, uintptr(unsafe.Pointer(&attr)), uintptr(unsafe.Pointer(&cargv[0])), uintptr(unsafe.Pointer(&cenv[0])))
	runtime.KeepAlive(cargv)
	runtime.KeepAlive(cenv)
	runtime.KeepAlive(path)
	return syscall.Errno(r)
}
