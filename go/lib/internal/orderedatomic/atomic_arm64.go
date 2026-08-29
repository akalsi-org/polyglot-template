//go:build linux && arm64

package orderedatomic

//go:noescape
func LoadRelaxed32(ptr *uint32) uint32

//go:noescape
func LoadAcquire32(ptr *uint32) uint32

//go:noescape
func CompareAndSwap32(ptr *uint32, old, new uint32) bool

//go:noescape
func LoadRelaxed64(ptr *uint64) uint64

//go:noescape
func LoadAcquire64(ptr *uint64) uint64

//go:noescape
func CompareAndSwap64(ptr *uint64, old, new uint64) bool

// CompareAndSwapAcquire64 has acquire ordering on success and relaxed ordering on failure.
//
//go:noescape
func CompareAndSwapAcquire64(ptr *uint64, old, new uint64) bool

//go:noescape
func FetchOrAcqRel64(ptr *uint64, value uint64) uint64

//go:noescape
func FetchAndRelease64(ptr *uint64, value uint64) uint64

//go:noescape
func FetchAddAcqRel32(ptr *uint32, value uint32) uint32
