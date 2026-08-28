// Package mpsc provides shared-memory SPSC and MPSC queues for Linux.
//
// The package supports amd64 and arm64 with CGO disabled.
// The package creates and attaches only shared-memory format version 4.
//
// Create functions own their queue mappings until Queue.Close succeeds.
// Attach functions duplicate the supplied descriptor and own the duplicate.
// Constructors create all producer and consumer handles.
// Handle zero values are not usable and return ErrMisuse.
//
// Each handle locks its goroutine to one OS thread.
// The lock consumes an OS thread until handle Close succeeds.
// Exact dead-thread recovery requires this identity.
// The caller must use and close the handle from its attaching goroutine.
// Hot operations do not verify this rule.
//
// MPSC permits concurrent producers, one consumer, and concurrent producer-consumer use.
// SPSC permits one producer, one consumer, and concurrent producer-consumer use.
// A successful Commit publishes the complete record before Peek returns it.
// A successful Pop makes retired capacity available to producers.
//
// Operations provide no general lock-free or wait-free guarantee.
// ErrFull and ErrContended are retryable admission results.
// ErrReaderDead reports proven reader death and requires caller recovery.
// ErrRecovered reports that reader recovery invalidated a producer reservation.
// Liveness checks and kernel operations can block on cold paths.
//
// Reserve, Commit, Abort, Peek, and Pop allocate no memory during steady-state use.
// Write copies one payload into mapped storage.
// CopyTo copies one payload out of mapped storage.
//
// A WriteSpan view remains valid until Commit, Abort, or producer Close.
// A ReadSpan view remains valid until Pop or consumer Close.
// Close auto-aborts a live WriteSpan.
// Consumer Close does not advance a live ReadSpan.
// Go cannot revoke a copied mapped slice.
// The caller must not retain or use a view after its invalidation event.
// Close all handles before closing their queue.
package mpsc
