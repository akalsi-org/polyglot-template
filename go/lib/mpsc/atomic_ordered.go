//go:build linux && (amd64 || arm64)

package mpsc

// cpuRelax gives the processor a spin-loop hint.
//
//go:noescape
func cpuRelax()

// SpinWait gives the processor a spin-loop hint during a caller-managed retry loop.
// It does not yield the OS thread, block, allocate, or access queue state.
func SpinWait() {
	cpuRelax()
}

func claimBackoffPauses(priorFailures uint32) uint32 {
	if priorFailures > 3 {
		priorFailures = 3
	}
	return 1 << priorFailures
}

func claimBackoff(priorFailures uint32) {
	for range claimBackoffPauses(priorFailures) {
		cpuRelax()
	}
}

//go:noescape
func storeRelaxed32(ptr *uint32, value uint32)

//go:noescape
func storeRelease32(ptr *uint32, value uint32)

//go:noescape
func storeRelaxed64(ptr *uint64, value uint64)

//go:noescape
func storeRelease64(ptr *uint64, value uint64)
