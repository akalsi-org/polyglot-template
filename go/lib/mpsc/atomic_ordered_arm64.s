//go:build linux && arm64

#include "textflag.h"

TEXT ·cpuRelax(SB), NOSPLIT, $0-0
	YIELD
	RET

TEXT ·loadRelaxed32(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	MOVWU (R0), R1
	MOVW R1, ret+8(FP)
	RET

TEXT ·loadAcquire32(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	LDARW (R0), R1
	MOVW R1, ret+8(FP)
	RET

TEXT ·storeRelaxed32(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	MOVW value+8(FP), R1
	MOVW R1, (R0)
	RET

TEXT ·storeRelease32(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	MOVW value+8(FP), R1
	STLRW R1, (R0)
	RET

TEXT ·compareAndSwap32(SB), NOSPLIT, $0-17
	MOVD ptr+0(FP), R0
	MOVW old+8(FP), R1
	MOVW new+12(FP), R2
cas32_loop:
	LDAXRW (R0), R3
	CMPW R1, R3
	BNE cas32_done
	STLXRW R2, (R0), R4
	CBNZ R4, cas32_loop
cas32_done:
	CSET EQ, R0
	MOVB R0, ret+16(FP)
	RET

TEXT ·loadRelaxed64(SB), NOSPLIT, $0-16
	MOVD ptr+0(FP), R0
	MOVD (R0), R1
	MOVD R1, ret+8(FP)
	RET

TEXT ·loadAcquire64(SB), NOSPLIT, $0-16
	MOVD ptr+0(FP), R0
	LDAR (R0), R1
	MOVD R1, ret+8(FP)
	RET

TEXT ·storeRelaxed64(SB), NOSPLIT, $0-16
	MOVD ptr+0(FP), R0
	MOVD value+8(FP), R1
	MOVD R1, (R0)
	RET

TEXT ·storeRelease64(SB), NOSPLIT, $0-16
	MOVD ptr+0(FP), R0
	MOVD value+8(FP), R1
	STLR R1, (R0)
	RET

TEXT ·compareAndSwap64(SB), NOSPLIT, $0-25
	MOVD ptr+0(FP), R0
	MOVD old+8(FP), R1
	MOVD new+16(FP), R2
cas64_loop:
	LDAXR (R0), R3
	CMP R1, R3
	BNE cas64_done
	STLXR R2, (R0), R4
	CBNZ R4, cas64_loop
cas64_done:
	CSET EQ, R0
	MOVB R0, ret+24(FP)
	RET

TEXT ·compareAndSwapAcquire64(SB), NOSPLIT, $0-25
	MOVD ptr+0(FP), R0
	MOVD old+8(FP), R1
	MOVD new+16(FP), R2
cas_acquire64_loop:
	LDXR (R0), R3
	CMP R1, R3
	BNE cas_acquire64_fail
	STXR R2, (R0), R4
	CBNZ R4, cas_acquire64_loop
	DMB $0x9
	MOVD $1, R0
	MOVB R0, ret+24(FP)
	RET
cas_acquire64_fail:
	CLREX
	MOVD $0, R0
	MOVB R0, ret+24(FP)
	RET

TEXT ·fetchOrAcqRel64(SB), NOSPLIT, $0-24
	MOVD ptr+0(FP), R0
	MOVD value+8(FP), R1
fetch_or_loop:
	LDAXR (R0), R2
	ORR R1, R2, R3
	STLXR R3, (R0), R4
	CBNZ R4, fetch_or_loop
	MOVD R2, ret+16(FP)
	RET

TEXT ·fetchAndRelease64(SB), NOSPLIT, $0-24
	MOVD ptr+0(FP), R0
	MOVD value+8(FP), R1
fetch_and_loop:
	LDXR (R0), R2
	AND R1, R2, R3
	STLXR R3, (R0), R4
	CBNZ R4, fetch_and_loop
	MOVD R2, ret+16(FP)
	RET

TEXT ·fetchAddAcqRel32(SB), NOSPLIT, $0-20
	MOVD ptr+0(FP), R0
	MOVW value+8(FP), R1
fetch_add32_loop:
	LDAXRW (R0), R2
	ADDW R1, R2, R3
	STLXRW R3, (R0), R4
	CBNZ R4, fetch_add32_loop
	MOVW R2, ret+16(FP)
	RET
