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

#include "pgt/core/types.hh"

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
// A policy is a HANDLE, not the state -- the allocator idiom. A queue copies its
// policy, so a stateful policy must hold a pointer to its shared counters and
// copy cheaply; a policy with bare mutable members gets silently duplicated per
// copy and its state fragments.
//
// BaselinePolicy defines every hook as a no-op; DefaultPolicy adds the promoted
// pause ladder. Policies are duck-typed, so one that implements only a subset
// still works -- the queue tests each hook with `requires` before calling it.
struct BaselinePolicy {
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
  // The two failure hooks make the experiment diagnostics attributable without
  // adding a shared counter to the claim path.  Instrumented policies must keep
  // their state writer-local (normally TLS) and aggregate after writers stop.
  void onClaim(u64_t /*pos*/, u64_t /*extent*/, u32_t /*hops*/) noexcept {}
  // prior_failures includes a preceding optimistic fast-path CAS failure when
  // the next failure happens in reserveSlow().
  void onClaimFailure(bool /*fast_path*/, u32_t /*prior_failures*/) noexcept {}
  void onCommit(u64_t /*pos*/, u64_t /*extent*/) noexcept {}
};

// The promoted production policy: post-success successor prefetch plus the
// explicit 1/2/4/8 CAS-failure pause ladder in Ring. The inherited contention
// hook must stay empty or it would add a ninth pause to that ladder.
struct DefaultPolicy : BaselinePolicy {
  // The explicit 1/2/4/8 CAS-failure pause ladder, owned by the queue rather
  // than by this hook. onContended must stay empty or it would add a ninth
  // pause to that documented sequence.
  static constexpr bool kCasFailureBackoff = true;
  void onContended(u32_t /*iter*/) noexcept {}
};

// Duck-typed selector for the pause ladder. Policies opt out by declaring
// kCasFailureBackoff = false; anything that does not declare it keeps the
// ladder, which is the promoted behaviour.
template <typename Policy>
inline constexpr bool kUsesCasFailureBackoff = [] {
  if constexpr (requires { Policy::kCasFailureBackoff; }) {
    return static_cast<bool>(Policy::kCasFailureBackoff);
  } else {
    return true;
  }
}();

// `prior_failures` counts every preceding failed claim attempt, including an
// optimistic fast-path failure before the slow walk begins. Keeping this
// mapping separate makes the 1/2/4/8 sequence mechanically testable.
[[nodiscard]] constexpr u32_t casFailureBackoffPauses(u32_t prior_failures) noexcept {
  return 1u << (prior_failures < 3 ? prior_failures : 3);
}

template <typename Policy>
inline void noteClaimFailure(Policy& policy, bool fast_path, u32_t prior_failures) noexcept {
  if constexpr (requires { policy.onClaimFailure(fast_path, prior_failures); }) {
    policy.onClaimFailure(fast_path, prior_failures);
  }
}

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
