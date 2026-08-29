//go:build linux && amd64 && !race

package atmc

// TSO makes ordinary stores release stores. Plain assignments keep these
// operations inlinable and avoid the XCHG used by sequentially consistent
// sync/atomic stores on amd64.

func StoreRelaxedU32(ptr *uint32, value uint32) { *ptr = value }
func StoreReleaseU32(ptr *uint32, value uint32) { *ptr = value }
func StoreRelaxedU64(ptr *uint64, value uint64) { *ptr = value }
func StoreReleaseU64(ptr *uint64, value uint64) { *ptr = value }
func StoreRelaxedI32(ptr *int32, value int32)   { *ptr = value }
func StoreReleaseI32(ptr *int32, value int32)   { *ptr = value }
func StoreRelaxedI64(ptr *int64, value int64)   { *ptr = value }
func StoreReleaseI64(ptr *int64, value int64)   { *ptr = value }
func StoreRelaxedUintptr(ptr *uintptr, value uintptr) {
	*ptr = value
}
func StoreReleaseUintptr(ptr *uintptr, value uintptr) {
	*ptr = value
}
