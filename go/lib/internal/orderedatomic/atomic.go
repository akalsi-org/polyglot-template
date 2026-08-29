//go:build linux && (amd64 || arm64)

// Package orderedatomic provides atomic operations with explicit memory ordering.
package orderedatomic

// Relax gives the processor a spin-loop hint.
//
//go:noescape
func Relax()

// StoreBarrier orders all prior stores before all subsequent stores.
//
//go:noescape
func StoreBarrier()

// LoadBarrier orders all prior loads before all subsequent loads.
//
//go:noescape
func LoadBarrier()

// FillRelaxed64 stores value in count contiguous uint64 elements starting at ptr.
// Each store is relaxed, and the range update is not atomic as a unit.
// Ptr must name a writable range for the duration of the call.
// A zero count does not access ptr.
//
//go:noescape
func FillRelaxed64(ptr *uint64, count uintptr, value uint64)

//go:noescape
func StoreRelaxed32(ptr *uint32, value uint32)

//go:noescape
func StoreRelease32(ptr *uint32, value uint32)

//go:noescape
func StoreRelaxed64(ptr *uint64, value uint64)

//go:noescape
func StoreRelease64(ptr *uint64, value uint64)
