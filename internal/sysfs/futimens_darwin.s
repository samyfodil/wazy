// lifted from golang.org/x/sys unix
#include "textflag.h"

TEXT libc_futimens_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_futimens(SB)

GLOBL ·libc_futimens_trampoline_addr(SB), RODATA, $8
DATA ·libc_futimens_trampoline_addr(SB)/8, $libc_futimens_trampoline<>(SB)

TEXT libc_utimensat_trampoline<>(SB), NOSPLIT, $0-0
	JMP libc_utimensat(SB)

GLOBL ·libc_utimensat_trampoline_addr(SB), RODATA, $8
DATA ·libc_utimensat_trampoline_addr(SB)/8, $libc_utimensat_trampoline<>(SB)
