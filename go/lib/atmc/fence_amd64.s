//go:build linux && amd64

#include "textflag.h"

TEXT ·Relax(SB), NOSPLIT, $0-0
	PAUSE
	RET

// The call boundary blocks compiler motion. TSO orders ordinary loads.
TEXT ·LoadBarrier(SB), NOSPLIT, $0-0
	RET

// The call boundary blocks compiler motion. TSO orders ordinary stores.
TEXT ·StoreBarrier(SB), NOSPLIT, $0-0
	RET

// TSO permits a store before a later load, so this fence must execute.
TEXT ·FullBarrier(SB), NOSPLIT, $0-0
	MFENCE
	RET
