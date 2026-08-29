//go:build linux && (arm64 || (amd64 && race))

package atmc

import "sync/atomic"

// On arm64, sync/atomic emits STLR and keeps these operations inlinable. The
// relaxed variants are stronger than requested. Race builds on amd64 also use
// these intrinsics so the race detector recognizes synchronization.

func StoreRelaxedU32(ptr *uint32, value uint32) { atomic.StoreUint32(ptr, value) }
func StoreReleaseU32(ptr *uint32, value uint32) { atomic.StoreUint32(ptr, value) }
func StoreRelaxedU64(ptr *uint64, value uint64) { atomic.StoreUint64(ptr, value) }
func StoreReleaseU64(ptr *uint64, value uint64) { atomic.StoreUint64(ptr, value) }
func StoreRelaxedI32(ptr *int32, value int32)   { atomic.StoreInt32(ptr, value) }
func StoreReleaseI32(ptr *int32, value int32)   { atomic.StoreInt32(ptr, value) }
func StoreRelaxedI64(ptr *int64, value int64)   { atomic.StoreInt64(ptr, value) }
func StoreReleaseI64(ptr *int64, value int64)   { atomic.StoreInt64(ptr, value) }
func StoreRelaxedUintptr(ptr *uintptr, value uintptr) {
	atomic.StoreUintptr(ptr, value)
}
func StoreReleaseUintptr(ptr *uintptr, value uintptr) {
	atomic.StoreUintptr(ptr, value)
}
