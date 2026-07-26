#pragma once

// Compile-time policy hooks, shared by every queue variant.
//
// These are template parameters with empty defaults, never virtual calls: several
// sit on the hot path and an unused hook must vanish entirely. An instrumented
// build is a different policy type, not a different queue.
//
// Wait policies receive the iteration count so a backoff ladder needs no state of
// its own. Event hooks are fire-and-forget and must not block.
//
// on_empty and on_busy must stay separate. `empty` means no data exists -- the
// frontier is reached and the wait may be arbitrarily long. `busy` means a record
// is in flight and arrives when its writer commits. Under the two-phase API that
// in-flight window is CALLER-controlled, not bounded by the queue: a writer may
// hold a reservation across arbitrary user code, so on_busy can legitimately last
// milliseconds and wants a full backoff ladder rather than a pure spin.
//
// Never use a shared counter for instrumentation. A contended atomic increment
// costs more than the claim CAS it would be measuring, and manufactures exactly
// the coherence traffic these queues are built to avoid. Counters must be
// thread-local and aggregated at teardown.

#include "../core/types.hh"

#if defined(__x86_64__)
#include <emmintrin.h>
#endif

namespace pgt::mpsc {

inline void cpuRelax() noexcept {
#if defined(__x86_64__)
  _mm_pause();
#elif defined(__aarch64__)
  __asm__ __volatile__("yield" ::: "memory");
#endif
}

// Every hook is a no-op. Suitable for a busy-poll deployment; note that
// `onBusy` still relaxes, because the reader's in-flight poll shares a cache
// line with the payload its owner is writing and an unrelaxed spin makes that
// line ping-pong.
//
// A policy is a HANDLE, not the state -- the allocator idiom. Queues copy their
// policy (Sharded copies one into each ring), so a stateful policy must hold a
// pointer to its shared counters and copy cheaply; a policy with bare mutable
// members gets silently duplicated per copy and its state fragments per shard.
struct DefaultPolicy {
  // Wait policies.
  void onEmpty(u32_t /*iter*/) noexcept {}
  void onBusy(u32_t /*iter*/) noexcept { cpuRelax(); }
  void onFull(u32_t /*iter*/) noexcept {}
  void onContended(u32_t /*iter*/) noexcept { cpuRelax(); }

  // Event hooks.
  //
  // onReclaim fires exactly when a writer was found dead and its record
  // recovered. That event is invisible otherwise, and a queue quietly recovering
  // from dying writers is something to alert on rather than discover later.
  void onReclaim(u64_t /*pos*/, u32_t /*tid*/, u64_t /*bytes*/) noexcept {}
  void onAbort(u64_t /*pos*/, u64_t /*bytes*/) noexcept {}
  void onWrap(u64_t /*lap*/) noexcept {}

  // Instrumentation. `hops` is the walk length -- the cheapest actionable metric
  // in the design, since it measures directly how stale the write hint is.
  void onClaim(u64_t /*pos*/, u64_t /*extent*/, u32_t /*hops*/) noexcept {}
  void onCommit(u64_t /*pos*/, u64_t /*extent*/) noexcept {}
};

// Exponential backoff ending in a yield. A reasonable default when the reader is
// not pinned to a dedicated core.
struct BackoffPolicy : DefaultPolicy {
  void onBusy(u32_t iter) noexcept { ladder(iter); }
  void onEmpty(u32_t iter) noexcept { ladder(iter); }
  void onContended(u32_t iter) noexcept { ladder(iter); }

 private:
  static void ladder(u32_t iter) noexcept;
};

}  // namespace pgt::mpsc
