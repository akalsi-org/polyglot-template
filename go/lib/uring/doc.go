//go:build linux && (amd64 || arm64)

// Package uring submits file and memory housekeeping to the kernel without
// blocking the calling thread.
//
// It exists for work that is advisory: preallocate a range, release page-table
// entries, start writeback, drop page cache. Such work has no deadline and no
// result the caller must read, but done synchronously it still costs the
// caller tens of microseconds per call, because the kernel does the work on
// the calling thread. A ring turns each call into a staged entry the kernel
// picks up on its own workers.
//
// A ring belongs to one goroutine. It is not safe for concurrent use. Callers
// that need one per thread should create one per thread; a ring is cheap.
//
// Staging never blocks. When the ring is full, the staging call reports false
// and the caller drops the work. That is the right answer for advisory work:
// blocking would put the caller back on the synchronous syscall the ring
// exists to avoid.
package uring
