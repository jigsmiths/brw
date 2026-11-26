#include "textflag.h"

TEXT libc_posix_spawn_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawn(SB)
GLOBL	·libc_posix_spawn_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawn_trampoline_addr(SB)/8, $libc_posix_spawn_trampoline<>(SB)

TEXT libc_posix_spawnattr_init_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_init(SB)
GLOBL	·libc_posix_spawnattr_init_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_init_trampoline_addr(SB)/8, $libc_posix_spawnattr_init_trampoline<>(SB)

TEXT libc_posix_spawnattr_setflags_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_setflags(SB)
GLOBL	·libc_posix_spawnattr_setflags_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_setflags_trampoline_addr(SB)/8, $libc_posix_spawnattr_setflags_trampoline<>(SB)

TEXT libc_posix_spawnattr_destroy_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_posix_spawnattr_destroy(SB)
GLOBL	·libc_posix_spawnattr_destroy_trampoline_addr(SB), RODATA, $8
DATA	·libc_posix_spawnattr_destroy_trampoline_addr(SB)/8, $libc_posix_spawnattr_destroy_trampoline<>(SB)

TEXT libc_responsibility_spawnattrs_setdisclaim_trampoline<>(SB),NOSPLIT,$0-0
	JMP	libc_responsibility_spawnattrs_setdisclaim(SB)
GLOBL	·libc_responsibility_spawnattrs_setdisclaim_trampoline_addr(SB), RODATA, $8
DATA	·libc_responsibility_spawnattrs_setdisclaim_trampoline_addr(SB)/8, $libc_responsibility_spawnattrs_setdisclaim_trampoline<>(SB)
