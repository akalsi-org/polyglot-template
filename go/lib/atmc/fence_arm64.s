//go:build linux && arm64

#include "textflag.h"

TEXT ·Relax(SB), NOSPLIT, $0-0
	YIELD
	RET

TEXT ·LoadBarrier(SB), NOSPLIT, $0-0
	DMB $0x9
	RET

TEXT ·StoreBarrier(SB), NOSPLIT, $0-0
	DMB $0xa
	RET

TEXT ·FullBarrier(SB), NOSPLIT, $0-0
	DMB $0xb
	RET
