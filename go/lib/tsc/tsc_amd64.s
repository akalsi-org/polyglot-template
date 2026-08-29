//go:build linux && amd64

#include "textflag.h"

// func Read() uint64
//
// LFENCE before RDTSC stops the processor executing the counter read before
// earlier instructions have retired. Without it the reading floats within a
// window of a few tens of cycles, which is the same size as the operations
// this counter exists to measure.
//
// LFENCE serializes dispatch on Intel, and on AMD from family 17h onward,
// where it is the documented default. Read is therefore an ordered boundary
// on both. ReadFast is the same read without the barrier.
TEXT ·Read(SB), NOSPLIT, $0-8
	LFENCE
	RDTSC
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ret+0(FP)
	RET

// func ReadFast() uint64
//
// RDTSC alone. The processor may move it relative to the code around it, so a
// single reading is not an exact boundary. Use it to sample a distribution
// cheaply, not to time one short region.
TEXT ·ReadFast(SB), NOSPLIT, $0-8
	RDTSC
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ret+0(FP)
	RET

// func ReadTagged() (ticks uint64, tag uint32)
//
// RDTSCP waits for earlier instructions to retire before it reads, so like
// LFENCE before RDTSC it cannot float backwards. It does not stop later
// instructions starting first, so it is the right instruction to close a
// timed region and needs a barrier after it to open one.
//
// It also returns IA32_TSC_AUX, which Linux sets to the processor number.
// That is the reason to prefer it: a delta between two readings taken on
// different processors is meaningless, and this is the only form that can
// detect the migration rather than silently reporting nonsense.
TEXT ·ReadTagged(SB), NOSPLIT, $0-12
	BYTE $0x0f
	BYTE $0x01
	BYTE $0xf9 // RDTSCP: EDX:EAX = counter, ECX = IA32_TSC_AUX
	SHLQ $32, DX
	ORQ  DX, AX
	MOVQ AX, ticks+0(FP)
	MOVL CX, tag+8(FP)
	RET
