//go:build linux && amd64

#include "textflag.h"

TEXT ·Relax(SB), NOSPLIT, $0-0
	PAUSE
	RET

// The call boundary blocks compiler motion. TSO orders ordinary stores.
// Callers must not use non-temporal stores with this barrier.
TEXT ·StoreBarrier(SB), NOSPLIT, $0-0
	RET

// The call boundary blocks compiler motion. TSO orders ordinary loads.
TEXT ·LoadBarrier(SB), NOSPLIT, $0-0
	RET

TEXT ·FillRelaxed64(SB), NOSPLIT, $0-24
	MOVQ ptr+0(FP), AX
	MOVQ count+8(FP), CX
	MOVQ value+16(FP), DX
	TESTQ CX, CX
	JE fill_relaxed64_done
fill_relaxed64_loop:
	MOVQ DX, (AX)
	ADDQ $8, AX
	DECQ CX
	JNE fill_relaxed64_loop
fill_relaxed64_done:
	RET

TEXT ·StoreRelaxed32(SB), NOSPLIT, $0-12
	MOVQ ptr+0(FP), AX
	MOVL value+8(FP), CX
	MOVL CX, (AX)
	RET

TEXT ·StoreRelease32(SB), NOSPLIT, $0-12
	MOVQ ptr+0(FP), AX
	MOVL value+8(FP), CX
	MOVL CX, (AX)
	RET

TEXT ·StoreRelaxed64(SB), NOSPLIT, $0-16
	MOVQ ptr+0(FP), AX
	MOVQ value+8(FP), CX
	MOVQ CX, (AX)
	RET

TEXT ·StoreRelease64(SB), NOSPLIT, $0-16
	MOVQ ptr+0(FP), AX
	MOVQ value+8(FP), CX
	MOVQ CX, (AX)
	RET
