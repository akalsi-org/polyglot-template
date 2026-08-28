//go:build linux && amd64 && go1.27 && !go1.28

#include "textflag.h"

// Verified from the pinned Go 1.27 runtime build's generated go_asm.h:
// g_m=48 and m_procid=64.
#define GO127_G_M_OFFSET 48
#define GO127_M_PROCID_OFFSET 64

TEXT ·currentThreadIdentityFast(SB), NOSPLIT, $0-16
	MOVQ GO127_G_M_OFFSET(R14), AX
	TESTQ AX, AX
	JE zero
	MOVQ GO127_M_PROCID_OFFSET(AX), CX
	MOVQ CX, ret+0(FP)
	MOVQ AX, ret1+8(FP)
	RET
zero:
	XORQ AX, AX
	MOVQ AX, ret+0(FP)
	MOVQ AX, ret1+8(FP)
	RET
