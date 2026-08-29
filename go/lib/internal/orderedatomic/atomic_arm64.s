//go:build linux && arm64

#include "textflag.h"

TEXT ·Relax(SB), NOSPLIT, $0-0
	YIELD
	RET

TEXT ·StoreBarrier(SB), NOSPLIT, $0-0
	DMB $0xa
	RET

TEXT ·LoadBarrier(SB), NOSPLIT, $0-0
	DMB $0x9
	RET

TEXT ·FillRelaxed64(SB), NOSPLIT, $0-24
	MOVD ptr+0(FP), R0
	MOVD count+8(FP), R1
	MOVD value+16(FP), R2
	CBZ R1, fill_relaxed64_done
fill_relaxed64_loop:
	MOVD R2, (R0)
	ADD $8, R0
	SUB $1, R1
	CBNZ R1, fill_relaxed64_loop
fill_relaxed64_done:
	RET

TEXT ·LoadRelaxed32(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	MOVWU (R0), R1
	MOVW R1, ret+8(FP)
	RET

TEXT ·LoadAcquire32(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	LDARW (R0), R1
	MOVW R1, ret+8(FP)
	RET

TEXT ·StoreRelaxed32(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	MOVW value+8(FP), R1
	MOVW R1, (R0)
	RET

TEXT ·StoreRelease32(SB), NOSPLIT, $0-12
	MOVD ptr+0(FP), R0
	MOVW value+8(FP), R1
	STLRW R1, (R0)
	RET

TEXT ·CompareAndSwap32(SB), NOSPLIT, $0-17
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

TEXT ·LoadRelaxed64(SB), NOSPLIT, $0-16
	MOVD ptr+0(FP), R0
	MOVD (R0), R1
	MOVD R1, ret+8(FP)
	RET

TEXT ·LoadAcquire64(SB), NOSPLIT, $0-16
	MOVD ptr+0(FP), R0
	LDAR (R0), R1
	MOVD R1, ret+8(FP)
	RET

TEXT ·StoreRelaxed64(SB), NOSPLIT, $0-16
	MOVD ptr+0(FP), R0
	MOVD value+8(FP), R1
	MOVD R1, (R0)
	RET

TEXT ·StoreRelease64(SB), NOSPLIT, $0-16
	MOVD ptr+0(FP), R0
	MOVD value+8(FP), R1
	STLR R1, (R0)
	RET

TEXT ·CompareAndSwap64(SB), NOSPLIT, $0-25
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

TEXT ·CompareAndSwapAcquire64(SB), NOSPLIT, $0-25
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

TEXT ·FetchOrAcqRel64(SB), NOSPLIT, $0-24
	MOVD ptr+0(FP), R0
	MOVD value+8(FP), R1
fetch_or_loop:
	LDAXR (R0), R2
	ORR R1, R2, R3
	STLXR R3, (R0), R4
	CBNZ R4, fetch_or_loop
	MOVD R2, ret+16(FP)
	RET

TEXT ·FetchAndRelease64(SB), NOSPLIT, $0-24
	MOVD ptr+0(FP), R0
	MOVD value+8(FP), R1
fetch_and_loop:
	LDXR (R0), R2
	AND R1, R2, R3
	STLXR R3, (R0), R4
	CBNZ R4, fetch_and_loop
	MOVD R2, ret+16(FP)
	RET

TEXT ·FetchAddAcqRel32(SB), NOSPLIT, $0-20
	MOVD ptr+0(FP), R0
	MOVW value+8(FP), R1
fetch_add32_loop:
	LDAXRW (R0), R2
	ADDW R1, R2, R3
	STLXRW R3, (R0), R4
	CBNZ R4, fetch_add32_loop
	MOVW R2, ret+16(FP)
	RET
