//go:build linux && arm64

package mpsc

//go:noescape
func loadRelaxed32(ptr *uint32) uint32

//go:noescape
func loadAcquire32(ptr *uint32) uint32

//go:noescape
func compareAndSwap32(ptr *uint32, old, new uint32) bool

//go:noescape
func loadRelaxed64(ptr *uint64) uint64

//go:noescape
func loadAcquire64(ptr *uint64) uint64

//go:noescape
func compareAndSwap64(ptr *uint64, old, new uint64) bool

// compareAndSwapAcquire64 has acquire ordering on success and relaxed ordering on failure.
//
//go:noescape
func compareAndSwapAcquire64(ptr *uint64, old, new uint64) bool

//go:noescape
func fetchOrAcqRel64(ptr *uint64, value uint64) uint64

//go:noescape
func fetchAndRelease64(ptr *uint64, value uint64) uint64

//go:noescape
func fetchAddAcqRel32(ptr *uint32, value uint32) uint32
