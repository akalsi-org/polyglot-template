//go:build linux && (amd64 || arm64)

package mpsc

import "github.com/akalsi-org/polyglot-template/go/lib/internal/orderedatomic"

func cpuRelax() { orderedatomic.Relax() }

// SpinWait gives the processor a spin-loop hint during a caller-managed retry loop.
// It does not yield the OS thread, block, allocate, or access queue state.
func SpinWait() {
	orderedatomic.Relax()
}

func claimBackoffPauses(priorFailures uint32) uint32 {
	if priorFailures > 3 {
		priorFailures = 3
	}
	return 1 << priorFailures
}

func claimBackoff(priorFailures uint32) {
	for range claimBackoffPauses(priorFailures) {
		orderedatomic.Relax()
	}
}

func loadRelaxed32(ptr *uint32) uint32         { return orderedatomic.LoadRelaxed32(ptr) }
func loadAcquire32(ptr *uint32) uint32         { return orderedatomic.LoadAcquire32(ptr) }
func storeRelaxed32(ptr *uint32, value uint32) { orderedatomic.StoreRelaxed32(ptr, value) }
func storeRelease32(ptr *uint32, value uint32) { orderedatomic.StoreRelease32(ptr, value) }
func compareAndSwap32(ptr *uint32, old, new uint32) bool {
	return orderedatomic.CompareAndSwap32(ptr, old, new)
}

func loadRelaxed64(ptr *uint64) uint64         { return orderedatomic.LoadRelaxed64(ptr) }
func loadAcquire64(ptr *uint64) uint64         { return orderedatomic.LoadAcquire64(ptr) }
func storeRelaxed64(ptr *uint64, value uint64) { orderedatomic.StoreRelaxed64(ptr, value) }
func storeRelease64(ptr *uint64, value uint64) { orderedatomic.StoreRelease64(ptr, value) }
func compareAndSwap64(ptr *uint64, old, new uint64) bool {
	return orderedatomic.CompareAndSwap64(ptr, old, new)
}
func compareAndSwapAcquire64(ptr *uint64, old, new uint64) bool {
	return orderedatomic.CompareAndSwapAcquire64(ptr, old, new)
}
func fetchOrAcqRel64(ptr *uint64, value uint64) uint64 {
	return orderedatomic.FetchOrAcqRel64(ptr, value)
}
func fetchAndRelease64(ptr *uint64, value uint64) uint64 {
	return orderedatomic.FetchAndRelease64(ptr, value)
}
func fetchAddAcqRel32(ptr *uint32, value uint32) uint32 {
	return orderedatomic.FetchAddAcqRel32(ptr, value)
}
