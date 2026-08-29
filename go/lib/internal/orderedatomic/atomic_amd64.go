//go:build linux && amd64

package orderedatomic

import "sync/atomic"

func LoadRelaxed32(ptr *uint32) uint32 { return atomic.LoadUint32(ptr) }
func LoadAcquire32(ptr *uint32) uint32 { return atomic.LoadUint32(ptr) }

func CompareAndSwap32(ptr *uint32, old, new uint32) bool {
	return atomic.CompareAndSwapUint32(ptr, old, new)
}

func LoadRelaxed64(ptr *uint64) uint64 { return atomic.LoadUint64(ptr) }
func LoadAcquire64(ptr *uint64) uint64 { return atomic.LoadUint64(ptr) }

func CompareAndSwap64(ptr *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(ptr, old, new)
}

func CompareAndSwapAcquire64(ptr *uint64, old, new uint64) bool {
	return atomic.CompareAndSwapUint64(ptr, old, new)
}

func FetchOrAcqRel64(ptr *uint64, value uint64) uint64 {
	return atomic.OrUint64(ptr, value)
}

func FetchAndRelease64(ptr *uint64, value uint64) uint64 {
	return atomic.AndUint64(ptr, value)
}

func FetchAddAcqRel32(ptr *uint32, value uint32) uint32 {
	return atomic.AddUint32(ptr, value) - value
}
