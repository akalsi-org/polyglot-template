//go:build linux && arm64 && go1.27 && !go1.28

#include "textflag.h"

// Verified from the pinned Go 1.27 runtime build's generated go_asm.h:
// g_m=48 and m_procid=64.
#define GO127_G_M_OFFSET 48
#define GO127_M_PROCID_OFFSET 64

TEXT ·currentThreadIdentityFast(SB), NOSPLIT, $0-16
	MOVD GO127_G_M_OFFSET(g), R0
	CMP $0, R0
	BEQ zero
	MOVD GO127_M_PROCID_OFFSET(R0), R1
	MOVD R1, ret+0(FP)
	MOVD R0, ret1+8(FP)
	RET
zero:
	MOVD $0, R0
	MOVD R0, ret+0(FP)
	MOVD R0, ret1+8(FP)
	RET
