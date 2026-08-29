//go:build linux && (amd64 || arm64)

// Package atmc provides atomic operations with explicit memory-ordering names.
//
// Relaxed loads and stores provide atomic access without a requested ordering
// relationship. On arm64, these operations use sync/atomic intrinsics and are
// therefore stronger than requested. This trade keeps every typed operation
// inlinable.
//
// Acquire operations prevent later memory operations from moving before the
// atomic operation. Release operations prevent earlier memory operations from
// moving after the atomic operation. AcqRel operations provide both guarantees.
// Compare-and-swap ordering names describe the successful operation. The
// implementation can provide stronger ordering on failure.
//
// Add operations return the new value. Swap, And, and Or operations return the
// old value. All typed operations are atomic, non-blocking, and allocate no
// memory. Callers must provide naturally aligned storage that remains valid for
// the full operation.
//
// LoadBarrier is an acquire fence. StoreBarrier is a release fence. FullBarrier
// is a sequentially consistent fence. Relax gives the processor a spin-loop
// hint. The standalone functions are calls because Go has no fence or spin-hint
// intrinsic.
package atmc
