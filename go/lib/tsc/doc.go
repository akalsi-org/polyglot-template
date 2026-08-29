//go:build linux && (amd64 || arm64)

// Package tsc reads the processor timestamp counter.
//
// It exists because time.Now costs about forty nanoseconds on this platform,
// which is the same order as the operations a low-latency benchmark wants to
// measure. Timing a twenty nanosecond operation with a forty nanosecond clock
// reports mostly the clock. The counter costs a few nanoseconds instead.
//
// The counter is only a valid clock where the processor keeps it running at a
// constant rate across frequency changes and idle states. Available reports
// that. Where it is false, callers must use time.Now and accept its cost.
//
// Read places a barrier before the counter read, so the reading is an ordered
// boundary rather than a value the processor may float by a few tens of
// cycles. That barrier is LFENCE on amd64 and ISB on arm64. Use Read to time
// a short region.
//
// ReadFast omits the barrier. It is cheaper and its reading is not an exact
// boundary. Use it to sample a distribution, where the error averages out and
// the saving does not.
package tsc
