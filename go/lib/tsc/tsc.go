//go:build linux && (amd64 || arm64)

package tsc

import (
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Read returns the processor timestamp counter, ordered against the code
// around it. See the package comment for what the barrier costs and buys.
func Read() uint64

// ReadFast returns the counter without an ordering barrier. It is cheaper and
// the reading may float by a few tens of cycles.
func ReadFast() uint64

var (
	once  sync.Once
	ticks uint64
	ok    bool
)

// Available reports whether the counter advances at a constant rate and keeps
// running while the processor idles. Only then is it a clock.
func Available() bool {
	once.Do(calibrate)
	return ok
}

// TicksPerSecond returns the calibrated counter frequency.
// It returns zero when the counter is not usable as a clock.
func TicksPerSecond() uint64 {
	once.Do(calibrate)
	if !ok {
		return 0
	}
	return ticks
}

// Nanos converts a counter delta to nanoseconds.
func Nanos(delta uint64) int64 {
	once.Do(calibrate)
	if !ok || ticks == 0 {
		return 0
	}
	return int64(float64(delta) / float64(ticks) * 1e9)
}

// Calibrate remeasures the frequency over the given window.
// A longer window gives a more accurate rate. Callers that need better than
// the default accuracy should call this once at startup.
func Calibrate(window time.Duration) {
	once.Do(calibrate)
	if !ok {
		return
	}
	ticks = measure(window)
}

func calibrate() {
	if !invariant() {
		return
	}
	ok = true
	ticks = measure(20 * time.Millisecond)
	if ticks == 0 {
		ok = false
	}
}

// measure counts ticks across a wall-clock window.
func measure(window time.Duration) uint64 {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	t0 := time.Now()
	c0 := Read()
	for time.Since(t0) < window {
	}
	elapsed := time.Since(t0)
	c1 := Read()
	if elapsed <= 0 || c1 <= c0 {
		return 0
	}
	return uint64(float64(c1-c0) / elapsed.Seconds())
}

// invariant reports whether the processor documents a constant, nonstop
// counter. Without both, the counter changes rate with frequency or stops in
// an idle state, and a delta means nothing.
func invariant() bool {
	if runtime.GOARCH == "arm64" {
		// The architecture defines CNTVCT_EL0 as a fixed-frequency counter.
		return true
	}
	raw, err := os.ReadFile("/proc/cpuinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "flags") {
			continue
		}
		return strings.Contains(line, "constant_tsc") &&
			strings.Contains(line, "nonstop_tsc")
	}
	return false
}
