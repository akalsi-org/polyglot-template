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
