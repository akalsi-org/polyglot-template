//go:build linux && amd64

package mpsc

import "sync/atomic"

func loadRelaxed32(ptr *uint32) uint32 { return atomic.LoadUint32(ptr) }
func loadAcquire32(ptr *uint32) uint32 { return atomic.LoadUint32(ptr) }

func compareAndSwap32(ptr *uint32, old, new uint32) bool {
	return atomic.CompareAndSwapUint32(ptr, old, new)
}

func loadRelaxed64(ptr *uint64) uint64 { return atomic.LoadUint64(ptr) }
func loadAcquire64(ptr *uint64) uint64 { return atomic.LoadUint64(ptr) }

func compareAndSwap64(ptr *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(ptr, old, new)
}

func compareAndSwapAcquire64(ptr *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(ptr, old, new)
}

func fetchOrAcqRel64(ptr *uint64, value uint64) uint64 {
	return atomic.OrUint64(ptr, value)
}

func fetchAndRelease64(ptr *uint64, value uint64) uint64 {
	return atomic.AndUint64(ptr, value)
}

func fetchAddAcqRel32(ptr *uint32, value uint32) uint32 {
	return atomic.AddUint32(ptr, value) - value
}
