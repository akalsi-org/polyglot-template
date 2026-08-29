//go:build linux && (amd64 || arm64)

package atmc

import "sync/atomic"

// The sync/atomic intrinsics inline on both supported architectures. On arm64,
// they make relaxed operations stronger than requested. This is correct and
// preserves the inlining guarantee without per-operation assembly calls.

func LoadRelaxedU32(ptr *uint32) uint32 { return atomic.LoadUint32(ptr) }
func LoadAcquireU32(ptr *uint32) uint32 { return atomic.LoadUint32(ptr) }
func CompareAndSwapRelaxedU32(ptr *uint32, old, new uint32) bool {
	return atomic.CompareAndSwapUint32(ptr, old, new)
}
func CompareAndSwapAcquireU32(ptr *uint32, old, new uint32) bool {
	return atomic.CompareAndSwapUint32(ptr, old, new)
}
func CompareAndSwapReleaseU32(ptr *uint32, old, new uint32) bool {
	return atomic.CompareAndSwapUint32(ptr, old, new)
}
func CompareAndSwapAcqRelU32(ptr *uint32, old, new uint32) bool {
	return atomic.CompareAndSwapUint32(ptr, old, new)
}
func SwapAcqRelU32(ptr *uint32, value uint32) uint32 { return atomic.SwapUint32(ptr, value) }
func AddRelaxedU32(ptr *uint32, delta uint32) uint32 { return atomic.AddUint32(ptr, delta) }
func AddAcqRelU32(ptr *uint32, delta uint32) uint32  { return atomic.AddUint32(ptr, delta) }
func AndAcqRelU32(ptr *uint32, value uint32) uint32  { return atomic.AndUint32(ptr, value) }
func OrAcqRelU32(ptr *uint32, value uint32) uint32   { return atomic.OrUint32(ptr, value) }

func LoadRelaxedU64(ptr *uint64) uint64 { return atomic.LoadUint64(ptr) }
func LoadAcquireU64(ptr *uint64) uint64 { return atomic.LoadUint64(ptr) }
func CompareAndSwapRelaxedU64(ptr *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(ptr, old, new)
}
func CompareAndSwapAcquireU64(ptr *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(ptr, old, new)
}
func CompareAndSwapReleaseU64(ptr *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(ptr, old, new)
}
func CompareAndSwapAcqRelU64(ptr *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(ptr, old, new)
}
func SwapAcqRelU64(ptr *uint64, value uint64) uint64 { return atomic.SwapUint64(ptr, value) }
func AddRelaxedU64(ptr *uint64, delta uint64) uint64 { return atomic.AddUint64(ptr, delta) }
func AddAcqRelU64(ptr *uint64, delta uint64) uint64  { return atomic.AddUint64(ptr, delta) }
func AndAcqRelU64(ptr *uint64, value uint64) uint64  { return atomic.AndUint64(ptr, value) }
func OrAcqRelU64(ptr *uint64, value uint64) uint64   { return atomic.OrUint64(ptr, value) }

func LoadRelaxedI32(ptr *int32) int32 { return atomic.LoadInt32(ptr) }
func LoadAcquireI32(ptr *int32) int32 { return atomic.LoadInt32(ptr) }
func CompareAndSwapRelaxedI32(ptr *int32, old, new int32) bool {
	return atomic.CompareAndSwapInt32(ptr, old, new)
}
func CompareAndSwapAcquireI32(ptr *int32, old, new int32) bool {
	return atomic.CompareAndSwapInt32(ptr, old, new)
}
func CompareAndSwapReleaseI32(ptr *int32, old, new int32) bool {
	return atomic.CompareAndSwapInt32(ptr, old, new)
}
func CompareAndSwapAcqRelI32(ptr *int32, old, new int32) bool {
	return atomic.CompareAndSwapInt32(ptr, old, new)
}
func SwapAcqRelI32(ptr *int32, value int32) int32 { return atomic.SwapInt32(ptr, value) }
func AddRelaxedI32(ptr *int32, delta int32) int32 { return atomic.AddInt32(ptr, delta) }
func AddAcqRelI32(ptr *int32, delta int32) int32  { return atomic.AddInt32(ptr, delta) }
func AndAcqRelI32(ptr *int32, value int32) int32  { return atomic.AndInt32(ptr, value) }
func OrAcqRelI32(ptr *int32, value int32) int32   { return atomic.OrInt32(ptr, value) }

func LoadRelaxedI64(ptr *int64) int64 { return atomic.LoadInt64(ptr) }
func LoadAcquireI64(ptr *int64) int64 { return atomic.LoadInt64(ptr) }
func CompareAndSwapRelaxedI64(ptr *int64, old, new int64) bool {
	return atomic.CompareAndSwapInt64(ptr, old, new)
}
func CompareAndSwapAcquireI64(ptr *int64, old, new int64) bool {
	return atomic.CompareAndSwapInt64(ptr, old, new)
}
func CompareAndSwapReleaseI64(ptr *int64, old, new int64) bool {
	return atomic.CompareAndSwapInt64(ptr, old, new)
}
func CompareAndSwapAcqRelI64(ptr *int64, old, new int64) bool {
	return atomic.CompareAndSwapInt64(ptr, old, new)
}
func SwapAcqRelI64(ptr *int64, value int64) int64 { return atomic.SwapInt64(ptr, value) }
func AddRelaxedI64(ptr *int64, delta int64) int64 { return atomic.AddInt64(ptr, delta) }
func AddAcqRelI64(ptr *int64, delta int64) int64  { return atomic.AddInt64(ptr, delta) }
func AndAcqRelI64(ptr *int64, value int64) int64  { return atomic.AndInt64(ptr, value) }
func OrAcqRelI64(ptr *int64, value int64) int64   { return atomic.OrInt64(ptr, value) }

func LoadRelaxedUintptr(ptr *uintptr) uintptr { return atomic.LoadUintptr(ptr) }
func LoadAcquireUintptr(ptr *uintptr) uintptr { return atomic.LoadUintptr(ptr) }
func CompareAndSwapRelaxedUintptr(ptr *uintptr, old, new uintptr) bool {
	return atomic.CompareAndSwapUintptr(ptr, old, new)
}
func CompareAndSwapAcquireUintptr(ptr *uintptr, old, new uintptr) bool {
	return atomic.CompareAndSwapUintptr(ptr, old, new)
}
func CompareAndSwapReleaseUintptr(ptr *uintptr, old, new uintptr) bool {
	return atomic.CompareAndSwapUintptr(ptr, old, new)
}
func CompareAndSwapAcqRelUintptr(ptr *uintptr, old, new uintptr) bool {
	return atomic.CompareAndSwapUintptr(ptr, old, new)
}
func SwapAcqRelUintptr(ptr *uintptr, value uintptr) uintptr { return atomic.SwapUintptr(ptr, value) }
func AddRelaxedUintptr(ptr *uintptr, delta uintptr) uintptr { return atomic.AddUintptr(ptr, delta) }
func AddAcqRelUintptr(ptr *uintptr, delta uintptr) uintptr  { return atomic.AddUintptr(ptr, delta) }
func AndAcqRelUintptr(ptr *uintptr, value uintptr) uintptr  { return atomic.AndUintptr(ptr, value) }
func OrAcqRelUintptr(ptr *uintptr, value uintptr) uintptr   { return atomic.OrUintptr(ptr, value) }
