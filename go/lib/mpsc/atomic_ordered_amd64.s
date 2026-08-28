//go:build linux && amd64

#include "textflag.h"

TEXT ·cpuRelax(SB), NOSPLIT, $0-0
	PAUSE
	RET

TEXT ·storeRelaxed32(SB), NOSPLIT, $0-12
	MOVQ ptr+0(FP), AX
	MOVL value+8(FP), CX
	MOVL CX, (AX)
	RET

TEXT ·storeRelease32(SB), NOSPLIT, $0-12
	MOVQ ptr+0(FP), AX
	MOVL value+8(FP), CX
	MOVL CX, (AX)
	RET

TEXT ·storeRelaxed64(SB), NOSPLIT, $0-16
	MOVQ ptr+0(FP), AX
	MOVQ value+8(FP), CX
	MOVQ CX, (AX)
	RET

TEXT ·storeRelease64(SB), NOSPLIT, $0-16
	MOVQ ptr+0(FP), AX
	MOVQ value+8(FP), CX
	MOVQ CX, (AX)
	RET
