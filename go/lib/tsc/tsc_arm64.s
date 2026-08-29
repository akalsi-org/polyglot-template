//go:build linux && arm64

#include "textflag.h"

// func Read() uint64
//
// ISB is the architecture's instruction synchronization barrier. It plays the
// part LFENCE plays on amd64: it stops the counter read being satisfied
// before earlier instructions complete. CNTVCT_EL0 is the virtual counter,
// readable from user space and running at a fixed frequency.
TEXT ·Read(SB), NOSPLIT, $0-8
	ISB  $15         // ISB SY
	WORD $0xd53be040 // MRS CNTVCT_EL0, R0
	MOVD R0, ret+0(FP)
	RET

// func ReadFast() uint64
//
// The counter read without the barrier. See the amd64 file.
TEXT ·ReadFast(SB), NOSPLIT, $0-8
	WORD $0xd53be040 // MRS CNTVCT_EL0, R0
	MOVD R0, ret+0(FP)
	RET

// func ReadTagged() (ticks uint64, tag uint32)
//
// The architecture has no instruction that returns the counter and the
// processor together, so the tag is always zero here. Callers that must
// detect a migration have to ask the operating system.
TEXT ·ReadTagged(SB), NOSPLIT, $0-12
	ISB  $15
	WORD $0xd53be040 // MRS CNTVCT_EL0, R0
	MOVD R0, ticks+0(FP)
	MOVW ZR, tag+8(FP)
	RET
