#pragma once

// The queue variants. All three share the QueueLike contract in api.hh and are
// interchangeable at the call site; sharding is a composition over a ring type,
// not a third parallel implementation:
//
//   Ring       one shared arena, many writers, one reader, total order (below).
//   SpscRing   one writer per ring, no claim arbitration (impl-spsc).
//   Sharded    K rings in one Region, writers assigned to shards (impl-spsc).
//
// ============================================================================
// SECTION: Ring -- shared-ring MPSC (owner: impl-queue)
// ============================================================================
//
// One ring, many writers, one reader, total order.
//
// Claim-by-CAS-on-descriptor, writer-stamps-successor, reader-only recovery.
// The protocol is specified in the algorithm document (revision 5, model-checked);
// the invariants that carry the safety argument are restated here because every
// one of them was violated by an earlier draft:
//
//   * A slot is claimable iff it reads EXACTLY freeWord(p) for the position p the
//     claimant believes it is at -- never "iff zero". Two unwrapped positions
//     differing by a multiple of the capacity are the same physical word, and the
//     position in the FREE encoding is what makes a writer stopped across a lap
//     boundary fail its claim deterministically instead of claiming an aliased
//     slot from a different lap.
//
//   * State vouches for the SUCCESSOR (I2). kClaimed means the successor slot is
//     not yet stamped and may hold stale payload; a walker must never advance
//     past it. kCleared and above mean the successor is stamped. Every
//     transition into kAborted -- including recovery -- stamps the successor
//     first, or kAborted lies and the next walker advances into garbage.
//
//   * `size` is the RESERVATION EXTENT and boundaries only refine (I1). commit
//     may shrink it; a short commit writes its trailer BEFORE shrinking, so an
//     observer of either size value computes a real boundary.
//
//   * Recovery is READER-ONLY. A writer reaching kClaimed backs off and retries
//     regardless of owner liveness; helping is unsound in a way no per-word CAS
//     discipline fixes (a stalled helper's authority over the successor slot
//     expires the moment anyone else completes recovery).
//
//   * recover() re-reads the descriptor AFTER the liveness check. The value
//     peek() loaded before checking liveness is a stale snapshot -- the owner can
//     complete the whole protocol and die in that gap, and acting on the snapshot
//     either stamps kAborted over a kCommitted record or stamps FREE over a later
//     writer's live claim. This is the defect the model check found after three
//     inspection passes missed it.
//
//   * A walk never continues from the claim CAS's failure value (read relaxed --
//     a kCleared word observed without acquire does not guarantee visibility of
//     the successor stamp). It restarts, which re-reads with acquire.
//
// All descriptor atomics go through the canonical (primary) alias of the arena:
// `arena + (pos & mask)` is always below capacity, so no atomic ever touches the
// mirror. Payload spans may extend into the mirror -- that is what it is for.
//
// PERFORMANCE SHAPE. reserve() is a small force-inlined fast path (hint accurate,
// slot free, capacity OK -> claim and return) over a noinline/cold walk path: the
// push path is called from application code with its own icache footprint, and a
// fat inline path evicts it. The successor descriptor line is prefetched at claim
// time -- q = p + need is known before the CAS, and that cold miss dominates the
// causal chain between consecutive claims.

#include "../core/types.hh"
#include "../mpsc/api.hh"
#include "../mpsc/desc.hh"
#include "../mpsc/liveness.hh"
#include "../mpsc/policy.hh"
#include "../mpsc/region.hh"

#include <pthread.h>

#include <atomic>
#include <bit>
#include <cassert>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <memory>
#include <type_traits>
#include <utility>
#include <vector>

namespace pgt::mpsc {

inline constexpr u64_t kInvalidPos = ~u64_t{0};

// Misuse trap for the silent-corruption paths -- double commit, grown commit,
// cross-thread/fork commit. These stay ON in release builds: each check is a
// predictable never-taken branch over thread-local state, and each failure mode
// is silent cross-process memory corruption (a double commit after the ring
// wraps stores over a live later-lap descriptor -- the delayed-stamper defect
// reintroduced through the API). That trade is not close. Wedge-only misuse
// (reserve-twice) stays a debug assert.
[[noreturn, gnu::noinline, gnu::cold]] inline void misuseTrap(char const* what) noexcept {
  std::fprintf(stderr, "pgt::mpsc: fatal API misuse: %s\n", what);
  std::abort();
}

// Hot-path force-inline, degraded under TSan. TSan erases inlined frames from
// race stacks, which forces cpp/test/tsan.supp into file-wide rules that shadow
// real races (a payload-vs-payload overlap would hide behind them); with the
// frames intact the suppressions can name exact functions. Zero cost to
// production builds -- the attribute is unchanged when TSan is off.
#if defined(__SANITIZE_THREAD__)
#define PGT_MPSC_TSAN 1
#elif defined(__has_feature)
#if __has_feature(thread_sanitizer)
#define PGT_MPSC_TSAN 1
#endif
#endif
#if defined(PGT_MPSC_TSAN)
#define PGT_MPSC_HOT
#else
#define PGT_MPSC_HOT [[gnu::always_inline]]
#endif

template <typename Policy = DefaultPolicy>
class Ring {
 public:
  Ring() = default;
  explicit Ring(Policy policy) noexcept : policy_(std::move(policy)) {}

  // Non-owning: operates on one shard of a Region owned by the caller (this is
  // how Sharded composes rings). The Region must outlive the Ring; a Ring is a
  // view plus per-endpoint cursors, and the protocol state lives entirely in
  // the mapping. The policy is copied -- policies are small stateless hook
  // bundles by design.
  Ring(Region const& region, u32_t shard_index, Policy& policy) noexcept : policy_(policy) {
    bind(region, shard_index);
  }
  Ring(Region const& region, u32_t shard_index) noexcept { bind(region, shard_index); }

  Ring(Ring&&) noexcept = default;
  Ring& operator=(Ring&&) noexcept = default;
  Ring(Ring const&) = delete;
  Ring& operator=(Ring const&) = delete;

  // Owning convenience: creates a 1-shard region and binds to it, so a bare
  // Ring works standalone with no wrapper layer on the hot path.
  [[nodiscard]] static bool create(Config const& cfg, Ring& out) noexcept {
    Config c = cfg;
    c.shards = 1;  // this variant is the single shared ring by definition
    if (!Region::create(c, out.owned_)) return false;
    out.bind(out.owned_, 0);
    // Construction stamps desc[0] = freeWord(0), which is 0 -- the zero-filled
    // arena already satisfies it. Every other slot is only reached via a stamp.
    return true;
  }

  // Owning convenience: attaches to an existing 1-shard region by descriptor.
  [[nodiscard]] static bool attach(int fd, Ring& out) noexcept {
    if (!Region::attach(fd, out.owned_)) return false;
    if (out.owned_.shardCount() != 1) return false;
    out.bind(out.owned_, 0);
    return true;
  }

  // ---- writer ----------------------------------------------------------------

  // Claims `extentFor(n)` bytes at the frontier. Returns an empty span on
  // failure; status() reports why. The span is valid only until commit()/abort().
  //
  // LIMITATION: reservation state is thread_local per template instantiation,
  // not per queue instance (per-instance thread_local does not exist in C++).
  // One thread may hold at most ONE in-flight reservation across ALL instances
  // of a given Ring<Policy>; reserving on ring B while holding a reservation on
  // ring A is trapped in BOTH build modes (release: misuseTrap; the same-ring
  // double reserve is a debug assert only, since it wedges rather than
  // corrupts). Workaround if simultaneous reservations are needed: give the
  // rings distinct Policy types, which gives them distinct instantiations and
  // therefore distinct reservation slots.
  PGT_MPSC_HOT inline WriteSpan reserve(sz_t n) noexcept {
    WriterTls& t = tls_;
    assert(t.p == kInvalidPos && "reserve while a reservation is held");  // misuse (d)
    if (t.owner_epoch != epoch_) [[unlikely]] {
      // Cross-instance misuse is different in kind from same-ring double
      // reserve: proceeding would orphan the other ring's kClaimed record
      // under a live owner that will never commit it -- PERMANENTLY wedging a
      // queue the caller was not even touching, undiagnosable in production.
      // The epoch makes it precisely detectable, so trap it in release too.
      if (t.p != kInvalidPos) [[unlikely]]
        misuseTrap("reserve while holding a reservation on another ring");
      t.owner_epoch = epoch_;
      t.read_cache = 0;  // conservative in the SAFE direction: forces a read_pos
                         // reload and can only produce a false full. The danger
                         // runs the other way -- a stale read_cache LARGER than
                         // this ring's true read_pos passes the capacity check
                         // it should fail, and the claim overwrites unconsumed
                         // records. Silent corruption, not a hang.
    }
    if (t.tid == 0) [[unlikely]] {
      // Once per thread. gettid() is a real syscall (glibc does not cache it),
      // so it must not sit on the steady-state path. Registering the atfork
      // handler here -- before this thread can hold any reservation -- is what
      // lets commit() enforce misuse (e) in release builds without a syscall.
      registerAtfork();
      t.tid = currentTid();
    }
    u64_t const need = extentFor(n);
    if (need + kGrain > cap_ || need > kMaxExtent) [[unlikely]] {
      t.status = Status::kTooLarge;
      return {};
    }
    // Fast path: CAS directly from the hint, with NO preliminary acquire load
    // of the descriptor. This is sound by Lemma 1 and must not be "fixed" by
    // restoring the load:
    //
    //   The CAS's expected value is freeWord(p) -- the FREE encoding that
    //   carries p itself -- so SUCCESS proves by content alone that p was the
    //   frontier, regardless of how p was obtained or how stale the hint was.
    //   The walk's acquire load exists so a walker may ADVANCE PAST a record,
    //   which requires observing the successor stamp its kCleared state vouches
    //   for; this path advances past nothing and reads nothing of the
    //   predecessor, so that acquire has no analogue here. The claim CAS's own
    //   success ordering (acquire) still fences the payload writes that follow.
    //   On FAILURE we restart into the slow path, which re-reads with acquire
    //   -- the failure value (read relaxed) is never used.
    u64_t const p = hintRef().load(std::memory_order_acquire);
    if (p >= t.read_cache && p + need + kGrain - t.read_cache <= cap_) [[likely]] {
      prefetchDesc(p + need);  // successor line: the cold miss between claims
      u64_t expected = freeWord(p);
      u64_t const mine = packRecord(need, 0, State::kClaimed, t.tid);
      if (descRef(p).compare_exchange_strong(expected, mine, std::memory_order_acquire,
                                             std::memory_order_relaxed)) [[likely]] {
        return finishClaim(p, need, n, mine, /*hops=*/0);
      }
      // NEVER continue from the failure value -- it was read relaxed. Restart.
    }
    return reserveSlow(n, need);
  }

  // Publishes up to `actual_n` payload bytes from exactly this reservation.
  void commit(WriteSpan reservation, sz_t actual_n) noexcept {
    WriterTls& t = tls_;
    // Misuse (b)/(c)/(e): unconditional in release, see misuseTrap. (e) is
    // enforced through (b) by construction, with no per-commit syscall: the
    // reservation is thread_local, so a cross-thread commit reads the calling
    // thread's OWN (poisoned) state, and a fork'd child's is cleared by the
    // atfork handler -- registered before any reservation can exist (see
    // reserve's tid-init branch). Both land in the first trap. The fresh
    // gettid() compare is debug-only because it is a syscall.
    if (t.p == kInvalidPos) [[unlikely]]
      misuseTrap("commit without a live reservation (double commit?)");  // (b), (e)
    if (t.owner_epoch != epoch_) [[unlikely]]
      misuseTrap("commit on a queue this thread holds no reservation on");
    if (reservation.data() != arena_ + ((t.p + kHeaderSize) & mask_) || reservation.size() != t.n)
      misuseTrap("commit with a span other than the live reservation");
    if (actual_n > t.n) [[unlikely]]
      misuseTrap("committed payload exceeds the reservation");
    assert(currentTid() == tidOf(t.word) && "commit across fork or from wrong thread");
    u64_t const used = extentFor(actual_n);
    if (used > t.need) [[unlikely]]
      misuseTrap("committed extent exceeds the reservation");  // (c)
    // Trailer FIRST, then shrink size (I1: boundaries only refine). Both are
    // published by the commit release below, so no observer sees a shrunk size
    // without a trailer. The trailer store is relaxed: no path reaches p + used
    // except through an acquire of the commit word.
    if (used < t.need) {
      descRef(t.p + used)
        .store(packRecord(t.need - used, 0, State::kAborted, t.tid), std::memory_order_relaxed);
    }
    descRef(t.p).store(
      packRecord(used, static_cast<u32_t>(actual_n & (kGrain - 1)), State::kCommitted, t.tid),
      std::memory_order_release);  // publishes the payload
    policy_.onCommit(t.p, used);
    t.p = kInvalidPos;  // poison
  }

  // Publishes the whole reservation as void. Safe with no trailer: the record
  // keeps its original extent and the successor was stamped during reserve().
  void abort() noexcept {
    WriterTls& t = tls_;
    // Same unconditional guards as commit(): an abort store through a stale
    // reservation is the identical corruption path.
    if (t.p == kInvalidPos) [[unlikely]]
      misuseTrap("abort without a live reservation (double abort?)");
    if (t.owner_epoch != epoch_) [[unlikely]]
      misuseTrap("abort on a queue this thread holds no reservation on");
    assert(currentTid() == tidOf(t.word) && "abort across fork or from wrong thread");
    descRef(t.p).store(withState(t.word, State::kAborted), std::memory_order_release);
    policy_.onAbort(t.p, t.need);
    t.p = kInvalidPos;  // poison
  }

  // Convenience: reserve + copy + commit.
  bool write(void const* data, sz_t n) noexcept {
    WriteSpan const s = reserve(n);
    if (s.data() == nullptr) return false;
    if (n != 0) std::memcpy(s.data(), data, n);
    commit(s, n);
    return true;
  }

  // Why the last reserve() on this thread returned empty. Thread-local.
  [[nodiscard]] Status status() const noexcept { return tls_.status; }

  // No registration limit: any thread may write to the shared ring.
  [[nodiscard]] u32_t maxWriters() const noexcept { return kUnboundedWriters; }

  // ---- reader ----------------------------------------------------------------

  // The next deliverable record, or empty if none is deliverable yet. The span
  // is valid only until pop().
  [[nodiscard]] ReadSpan peek() noexcept {
    for (;;) {
      u64_t d = descRef(rd_).load(std::memory_order_acquire);  // pairs with commit release
      switch (stateOf(d)) {
        case State::kFree:
          return {};  // frontier
        case State::kAborted:
          peek_extent_ = extentOf(d);
          pop();
          continue;
        case State::kCommitted:
          peek_extent_ = extentOf(d);
          return {arena_ + ((rd_ + kHeaderSize) & mask_), committedLen(d)};
        case State::kClaimed:
        case State::kCleared: {
          // Liveness stays OFF the busy path. threadAlive() is an open/read/
          // close of /proc/<tid>/stat; consulting it on every busy poll turns a
          // preempted writer into a syscall-bound reader (measured ~450 rec/s
          // with 36s of sys time under oversubscription). Instead the check is
          // SPACED: only after kBusyPollsPerLiveness consecutive busy
          // observations of the same record, streak reset on any progress.
          // This is check-spacing, NOT a timeout -- no wall-clock constant
          // exists, exact-liveness semantics are unchanged, and a dead writer's
          // recovery is delayed by a bounded spin, not by time.
          if (rd_ != busy_pos_) {
            busy_pos_ = rd_;
            busy_streak_ = 0;
            busy_threshold_ = kBusyPollsPerLiveness;
          }
          if (++busy_streak_ < busy_threshold_) {
            policy_.onBusy(busy_streak_);
            return {};
          }
          busy_streak_ = 0;
          if (threadAlive(tidOf(d))) {
            // Alive verdict: re-space GEOMETRICALLY (128, 256, ... capped). A
            // record held across a whole scheduling quantum would otherwise
            // cost a /proc call every ~100ns of spin; doubling keeps the
            // first-check latency while flattening the steady-state syscall
            // rate. Dead-writer recovery latency is unaffected -- growth
            // happens only on alive verdicts, and any progress resets it.
            if (busy_threshold_ < kBusyPollsPerLivenessMax) busy_threshold_ *= 2;
            policy_.onBusy(busy_threshold_);
            return {};
          }
          // MUST re-read. Between the load of d and the liveness check the owner
          // can have finished the protocol AND died, so d is a stale snapshot. A
          // dead owner's word is frozen -- no writer helps, and no one can claim
          // a slot that does not read FREE -- so this re-read is final.
          d = descRef(rd_).load(std::memory_order_acquire);
          State const s = stateOf(d);
          if (s == State::kClaimed || s == State::kCleared) recover(rd_, d);
          continue;  // re-dispatch on the fresh value
        }
      }
    }
  }

  // Cheap sweep probe: one relaxed load, no liveness check, reader thread only.
  // False means the cursor is at the frontier and peek() would return empty.
  // True means there is SOMETHING at the cursor -- a deliverable or aborted
  // record, or one still in flight -- so a true result does not promise that
  // peek() returns data, and peek() on an in-flight record may consult /proc.
  //
  // Reporting true for in-flight records is deliberate, not imprecision: a
  // probe that only reported >= kCommitted would hide a dead writer's record
  // from the sweep FOREVER, because recovery only happens inside peek() -- the
  // ring would wedge with no one ever looking at it. A liveness-blind
  // "something is here" is the right primitive; sweeps that must avoid the
  // /proc cost on every pass should rotate, not spin, over rings whose probe
  // is true but whose peek() comes back empty.
  [[nodiscard]] bool maybeReadable() const noexcept {
    return stateOf(descRef(rd_).load(std::memory_order_relaxed)) != State::kFree;
  }

  // Retires the record at the read cursor and licenses overwrite of its bytes.
  void pop() noexcept {
    u64_t const prev = rd_;
    u64_t const extent = peek_extent_ != 0 ? peek_extent_
                                            : extentOf(descRef(rd_).load(std::memory_order_relaxed));
    peek_extent_ = 0;
    rd_ += extent;  // rd_ is reader-private
    readPosRef().store(rd_, std::memory_order_release);             // license to overwrite
    if ((prev ^ rd_) >> shift_) policy_.onWrap(rd_ >> shift_);
  }

  // ---- attachment (cold) -----------------------------------------------------

  [[nodiscard]] bool attachWriter() noexcept {
    if (arena_ == nullptr) return false;
    registerAtfork();
    return true;
  }

  // Enforces exactly one reader; takes over from a proven-dead predecessor,
  // resuming exactly at its published read position (the reader keeps no state
  // that is not in the control page, and recover() is idempotent).
  [[nodiscard]] bool attachReader() noexcept {
    if (arena_ == nullptr) return false;
    auto rt = readerTidRef();
    u32_t const self = currentTid();
    u32_t cur = rt.load(std::memory_order_acquire);
    // Re-attach by the current holder is idempotent: re-sync the cursor and
    // succeed. Sharded attaches ring by ring and reader_tid is queue-wide, so
    // without this shard 0 claims the role and shards 1..K-1 then see a live
    // reader -- themselves -- and fail. SpscRing has the same semantic; it is
    // part of the RingT contract Sharded relies on.
    if (cur != self) {
      if (cur != 0 && threadAlive(cur)) return false;  // a live reader exists
      if (!rt.compare_exchange_strong(cur, self, std::memory_order_acq_rel,
                                      std::memory_order_relaxed)) {
        return false;  // lost the takeover race
      }
    }
    rd_ = readPosRef().load(std::memory_order_acquire);  // resume exactly here
    peek_extent_ = 0;
    return true;
  }

  void detachWriter() noexcept {
    // RAII backstop for misuse (a): a reservation abandoned by a live thread
    // wedges the queue; abort it on the way out.
    if (tls_.p != kInvalidPos && tls_.owner_epoch == epoch_) abort();
  }

  // The owned region. Valid only for rings built via create()/attach(); a ring
  // bound over a caller-owned Region reports an invalid (empty) Region here.
  [[nodiscard]] Region const& region() const noexcept { return owned_; }
  [[nodiscard]] Policy& policy() noexcept { return policy_; }

 private:
  // Writer-private reservation state. thread_local: a writer holds at most one
  // reservation at a time, and the owner EPOCH catches a thread interleaving
  // two queues. An epoch, deliberately not a pointer: a Ring destroyed and a
  // new one constructed at the SAME address must not inherit the old TLS -- a
  // stale read_cache from a long-lived predecessor makes every reserve on the
  // fresh ring restart as a "stale walk" forever (found by the recreated-at-
  // reused-address regression test). Every bind() draws a fresh epoch, so
  // address reuse always mismatches; a moved Ring is a NEW binding of the same
  // protocol state, and re-priming read_cache from 0 is merely conservative.
  struct WriterTls {
    u64_t p = kInvalidPos;  // claimed position; kInvalidPos when not held
    u64_t need = 0;         // reservation extent
    u64_t n = 0;            // requested payload length
    u64_t word = 0;         // the claim word we wrote (carries our tid)
    u64_t read_cache = 0;   // last observed read_pos; may lag, never leads
    u64_t owner_epoch = 0;  // matches epoch_ of the ring this state belongs to
    u32_t tid = 0;          // cached gettid(); cleared across fork
    Status status = Status::kOk;
  };

  inline static thread_local WriterTls tls_{};

  static void atforkChild() noexcept { tls_ = WriterTls{}; }

  // Misuse (e): a fork'd child inherits the mapping AND the thread-local
  // reservation; clearing the latter in the child means both cannot commit --
  // the child's commit lands in the (b) trap. Called before any reservation
  // can exist so the release-mode guard has no window.
  //
  // FORK INVARIANT (queue-wide): after fork, a child holds NO reservation and
  // NO writer slot; it must re-register before writing. This handler clears
  // only Ring's reservation state. Any layer that adds MORE thread-local
  // binding -- Sharded's shard assignment, SpscRing's slot claim -- must
  // register its OWN atfork child handler clearing that binding, or a forked
  // child writes into the parent's ring through the inherited assignment.
  static void registerAtfork() noexcept {
    static std::once_flag once;
    std::call_once(once, [] { ::pthread_atfork(nullptr, nullptr, &atforkChild); });
  }

  void bind(Region const& region, u32_t shard_index) noexcept {
    ctl_ = region.control();
    sc_ = region.shard(shard_index);
    arena_ = region.arena(shard_index);
    cap_ = region.capacity();
    mask_ = region.mask();
    shift_ = static_cast<u32_t>(std::countr_zero(cap_));
    // Fresh, never-zero epoch per binding: see WriterTls. Zero is reserved so
    // an unbound ring (epoch_ 0) can never match a fresh thread's TLS.
    static std::atomic<u64_t> ctr{0};
    epoch_ = ctr.fetch_add(1, std::memory_order_relaxed) + 1;
  }

  // All descriptor atomics go through the canonical alias (pos & mask_ < cap_),
  // never the mirror -- the two aliases are different locations to the compiler.
  [[nodiscard]] std::atomic_ref<u64_t> descRef(u64_t pos) const noexcept {
    return std::atomic_ref<u64_t>(*reinterpret_cast<u64_t*>(arena_ + (pos & mask_)));
  }
  [[nodiscard]] std::atomic_ref<u64_t> hintRef() const noexcept {
    return std::atomic_ref<u64_t>(sc_->writeHint());  // hint contract, not publishPos()
  }
  [[nodiscard]] std::atomic_ref<u64_t> readPosRef() const noexcept {
    return std::atomic_ref<u64_t>(sc_->read_pos);
  }
  [[nodiscard]] std::atomic_ref<u32_t> readerTidRef() const noexcept {
    return std::atomic_ref<u32_t>(ctl_->reader_tid);
  }
  void prefetchDesc(u64_t pos) const noexcept {
    // PGT_MPSC_NO_PREFETCH exists for A/B benchmarking only; never define it in
    // production builds.
#ifndef PGT_MPSC_NO_PREFETCH
    __builtin_prefetch(arena_ + (pos & mask_), /*rw=*/1, /*locality=*/3);
#else
    static_cast<void>(pos);
#endif
  }

  // Post-claim protocol, shared by both claim paths. Small enough to inline.
  PGT_MPSC_HOT inline WriteSpan finishClaim(u64_t p, u64_t need, sz_t n, u64_t mine,
                                                      u32_t hops) noexcept {
    u64_t const q = p + need;
    // Stamp the successor as FREE(q). CAS from observed -- belt and braces; by
    // Lemma 2 no concurrent mutator of this slot can exist. Relaxed: publishes
    // no data, and the vouch release below is what makes it visible.
    auto succ = descRef(q);
    u64_t g = succ.load(std::memory_order_relaxed);
    succ.compare_exchange_strong(g, freeWord(q), std::memory_order_relaxed,
                                 std::memory_order_relaxed);
    descRef(p).store(withState(mine, State::kCleared), std::memory_order_release);  // vouch (I2)
    hintRef().store(q, std::memory_order_release);  // release, NOT relaxed -- see spec
    tls_.p = p;
    tls_.need = need;
    tls_.n = n;
    tls_.word = mine;
    tls_.status = Status::kOk;
    policy_.onClaim(p, need, hops);
    return {arena_ + ((p + kHeaderSize) & mask_), n};
  }

  // The walk. Out of line and cold: the fast path above is what belongs in the
  // caller's icache.
  [[gnu::noinline, gnu::cold]] WriteSpan reserveSlow(sz_t n, u64_t need) noexcept {
    WriterTls& t = tls_;
    u32_t const max_hops = static_cast<u32_t>(cap_ / kGrain);
    u32_t contended = 0;
    for (;;) {
      u64_t p = hintRef().load(std::memory_order_acquire);
      u32_t hops = 0;
      for (;;) {                              // walk to the true frontier
        if (++hops > max_hops) goto restart;  // bounded: stale walk
        u64_t const d = descRef(p).load(std::memory_order_acquire);
        State const s = stateOf(d);
        if (s == State::kFree) {
          if (!isFreeFor(d, p)) goto restart;  // recycled slot, stale walk
          break;                               // frontier
        }
        if (s == State::kClaimed) {  // owner alive, or the reader will recover
          policy_.onBusy(hops);
          goto restart;
        }
        p += extentOf(d);  // vouched by I2
      }

      if (p < t.read_cache) goto restart;  // stale walk, NOT full
      if (p + need + kGrain - t.read_cache > cap_) {
        t.read_cache = readPosRef().load(std::memory_order_acquire);
        if (p < t.read_cache) goto restart;
        if (p + need + kGrain - t.read_cache > cap_) {
          policy_.onFull(0);
          // Full is normal backpressure -- but a DEAD reader means it will never
          // drain, so report that distinctly rather than looping forever. An
          // unattached reader (tid 0) may yet attach, so it counts as full.
          u32_t const reader = readerTidRef().load(std::memory_order_acquire);
          t.status = (reader != 0 && !threadAlive(reader)) ? Status::kReaderDead : Status::kFull;
          return {};  // fail fast; the caller owns the retry policy
        }
      }

      {
        // Claim and identify in ONE atomic. The expected value carries p, so a
        // stale claimant from an earlier lap fails deterministically (Lemma 1).
        prefetchDesc(p + need);
        u64_t expected = freeWord(p);
        u64_t const mine = packRecord(need, 0, State::kClaimed, t.tid);
        if (descRef(p).compare_exchange_strong(expected, mine, std::memory_order_acquire,
                                               std::memory_order_relaxed)) {
          return finishClaim(p, need, n, mine, hops);
        }
        policy_.onContended(contended++);
        // Failure value was read relaxed: restart, never continue the walk.
      }
    restart:;
    }
  }

  // Reader-only recovery of a proven-dead writer's record. `d` MUST be a value
  // read AFTER the liveness check.
  [[gnu::noinline, gnu::cold]] void recover(u64_t p, u64_t d) noexcept {
    if (stateOf(d) == State::kClaimed) {  // owner never stamped its successor
      u64_t const q = p + extentOf(d);    // exact boundary, from I1
      auto succ = descRef(q);
      u64_t g = succ.load(std::memory_order_relaxed);
      bool const ok = succ.compare_exchange_strong(g, freeWord(q), std::memory_order_relaxed,
                                                   std::memory_order_relaxed);
      assert(ok && "recovery raced a mutator that Lemma 2 says cannot exist");
      static_cast<void>(ok);
    }
    // Successor stamped FIRST, and this store is release: a walker's acquire
    // load of kAborted must observe the stamp above, or I2 lies.
    descRef(p).store(withState(d, State::kAborted), std::memory_order_release);
    policy_.onReclaim(p, tidOf(d), extentOf(d));
  }

  Region owned_;  // set only by create()/attach(); empty for a shard-bound ring
  Control* ctl_ = nullptr;
  ShardControl* sc_ = nullptr;
  std::byte* arena_ = nullptr;
  u64_t cap_ = 0;
  u64_t mask_ = 0;
  u32_t shift_ = 0;
  u64_t epoch_ = 0;  // binding identity for WriterTls; 0 only while unbound
  u64_t rd_ = 0;     // reader-private cursor; a cache of read_pos
  u64_t peek_extent_ = 0;  // extent acquired by the successful peek

  // Liveness check spacing for the reader's busy path (see peek). Iterations,
  // not time: 128 relaxed polls of an L1-resident line is well under a live
  // writer's typical reserve-to-commit window, so a live writer normally
  // commits before the reader ever pays for /proc.
  static constexpr u32_t kBusyPollsPerLiveness = 128;
  static constexpr u32_t kBusyPollsPerLivenessMax = 1u << 16;  // cap on re-spacing
  u64_t busy_pos_ = kInvalidPos;  // record the busy streak is counting against
  u32_t busy_streak_ = 0;         // consecutive busy polls at busy_pos_
  u32_t busy_threshold_ = kBusyPollsPerLiveness;  // doubles per alive verdict

  [[no_unique_address]] Policy policy_{};
};

// ============================================================================
// SECTION: SpscRing -- one writer per ring, no claim arbitration (owner:
// impl-spsc)
// ============================================================================
//
// The degenerate case of Ring in which claim arbitration, and with it most of
// the protocol, disappears. The load-bearing change: the tail
// (ShardControl::publishPos()) is a PUBLICATION cursor, not a reservation
// cursor -- it advances only at commit, so the reader consumes [read_pos, tail)
// and everything in that range is complete by definition. What that deletes:
//
//   claim CAS           the sole writer's private tail IS the frontier, so
//                       reserve() is a bounds check plus writer-private
//                       bookkeeping, touching NOTHING shared. WAIT-FREE,
//                       strictly stronger than Ring's lock-free claim.
//
//   walk, write hint, FREE(p) self-certification
//                       existed so concurrent claimants could locate and prove
//                       the frontier; a sole writer has nothing to prove.
//
//   successor stamp, vouch (I2)
//                       walkers derived boundaries from descriptors ahead of
//                       publication; this reader never reads past the tail, so
//                       bytes beyond it are unreachable, stale or not.
//
//   recovery, read-path liveness
//                       an unfinished reservation has touched nothing shared
//                       (header store and tail advance both happen at commit),
//                       so a dead writer's ring simply idles: its committed
//                       records drain normally and nothing is behind an
//                       incomplete one. The r5 stale-snapshot defect class
//                       cannot arise -- the reader never dispatches on writer
//                       state at all.
//
//   kAborted records    abort() and the unused remainder of a short commit
//                       were never published; both return to the writer's
//                       private free space. abort() is purely writer-local.
//
// The reader pays ONE acquire load of the tail per sweep -- skipped while the
// cached tail still covers unread records -- and reads headers below it
// relaxed. Per-ring FIFO only; ordering across writers is a property of the
// variant (api.hh), and this is the variant that trades it for wait-freedom.
//
// Sole ownership is arbitrated through the Region's SHARED writer registry
// (slot index == shard index), so the claim is valid cross-process. Writer
// cursors live in plain members rather than TLS: there is exactly one writer,
// and members spare the hot path a TLS lookup.

template <typename Policy = DefaultPolicy>
class SpscRing {
 public:
  // One writer per ring. Sharded reads this to pick its registration path.
  static constexpr u32_t kMaxWriters = 1;

  SpscRing() = default;

  // Non-owning shard bind, mirroring Ring: the Region must outlive the ring.
  SpscRing(Region const& region, u32_t shard_index, Policy& policy) noexcept : policy_(policy) {
    bind(region, shard_index);
  }
  SpscRing(Region const& region, u32_t shard_index) noexcept { bind(region, shard_index); }

  SpscRing(SpscRing&& other) noexcept { moveFrom(std::move(other)); }
  SpscRing& operator=(SpscRing&& other) noexcept {
    if (this != &other) {
      if (ownsWriter()) misuseTrap("moving over an attached SpscRing");
      moveFrom(std::move(other));
    }
    return *this;
  }
  SpscRing(SpscRing const&) = delete;
  SpscRing& operator=(SpscRing const&) = delete;

  // Owning convenience: a standalone SPSC byte queue over its own 1-shard
  // region.
  [[nodiscard]] static bool create(Config const& cfg, SpscRing& out) noexcept {
    Config c = cfg;
    c.shards = 1;
    if (!Region::create(c, out.owned_)) return false;
    out.bind(out.owned_, 0);
    return true;
  }
  [[nodiscard]] static bool attach(int fd, SpscRing& out) noexcept {
    if (!Region::attach(fd, out.owned_)) return false;
    if (out.owned_.shardCount() != 1) return false;
    out.bind(out.owned_, 0);
    return true;
  }

  // ---- writer ----------------------------------------------------------------

  // WAIT-FREE: a bounds check against the cached read position and
  // writer-private bookkeeping. No shared load or store on the fast path --
  // the ring is not touched until commit().
  [[nodiscard, gnu::always_inline]] inline WriteSpan reserve(sz_t n) noexcept {
    assert(res_pos_ == kInvalidPos && "reserve while a reservation is held");  // misuse (d)
    if (!ownsWriter()) [[unlikely]]
      misuseTrap("SpscRing::reserve by a thread that does not own this ring");
    u64_t const need = extentFor(n);
    if (need > max_need_) [[unlikely]] {
      wr_status_ = Status::kTooLarge;
      return {};
    }
    u64_t const tail = wr_tail_;
    if (tail + need - wr_read_cache_ > cap_) [[unlikely]]
      return reserveSlow(n, need);
    res_pos_ = tail;
    res_need_ = need;
    res_n_ = n;
    wr_status_ = Status::kOk;
    return {arena_ + ((tail + kHeaderSize) & mask_), n};
  }

  // Misuse traps, ON in release (Ring's doctrine): each failure mode is silent
  // cross-process corruption -- a double commit republishes a stale
  // reservation, a grown commit publishes a fictitious boundary -- and each
  // check is a predictable never-taken branch over writer-private state.
  [[gnu::always_inline]] inline void commit(WriteSpan reservation, sz_t actual_n) noexcept {
    if (!ownsWriter()) [[unlikely]]
      misuseTrap("SpscRing::commit by a thread that does not own this ring");
    if (res_pos_ == kInvalidPos) [[unlikely]]
      misuseTrap("SpscRing::commit without a live reservation");  // misuse (b)
    if (reservation.data() != arena_ + ((res_pos_ + kHeaderSize) & mask_) ||
        reservation.size() != res_n_)
      misuseTrap("SpscRing::commit with a span other than the live reservation");
    if (actual_n > res_n_) [[unlikely]]
      misuseTrap("SpscRing::committed payload exceeds the reservation");
    u64_t const used = extentFor(actual_n);
    if (used > res_need_) [[unlikely]]
      misuseTrap("SpscRing::commit extent exceeds the reservation");  // misuse (c)
    u64_t const p = res_pos_;
    // Relaxed: the reader cannot reach this header until the tail release
    // below publishes it together with the payload.
    descRef(p).store(
      packRecord(used, static_cast<u32_t>(actual_n & (kGrain - 1)), State::kCommitted, wr_tid_),
      std::memory_order_relaxed);
    wr_tail_ = p + used;
    publishRef().store(wr_tail_, std::memory_order_release);  // publication
    res_pos_ = kInvalidPos;                                   // poison
    policy_.onCommit(p, used);
    // A short commit needs no trailer: the unused remainder was never
    // published, so it returns to this writer's private free space instead of
    // becoming a dead record the reader must skip.
  }

  void abort() noexcept {
    if (!ownsWriter()) [[unlikely]]
      misuseTrap("SpscRing::abort by a thread that does not own this ring");
    if (res_pos_ == kInvalidPos) [[unlikely]]
      misuseTrap("SpscRing::abort without a live reservation");
    policy_.onAbort(res_pos_, res_need_);
    // Nothing was published -- the tail never moved -- so aborting is purely
    // writer-local. No kAborted record exists for the reader to skip.
    res_pos_ = kInvalidPos;  // poison
  }

  bool write(void const* data, sz_t n) noexcept {
    WriteSpan const s = reserve(n);
    if (s.data() == nullptr) return false;
    if (n != 0) std::memcpy(s.data(), data, n);
    commit(s, n);
    return true;
  }

  [[nodiscard]] Status status() const noexcept { return wr_status_; }

  // Exclusive registration: exactly one writer, ever.
  [[nodiscard]] u32_t maxWriters() const noexcept { return kMaxWriters; }

  // ---- reader ----------------------------------------------------------------

  [[nodiscard]] ReadSpan peek() noexcept {
    u64_t const rd = rd_;
    if (rd >= tail_cache_) {
      // The ONE acquire of the sweep: its pairing release covers every header
      // and payload store at or below the tail, so records under the cached
      // tail are read relaxed until the cache is exhausted.
      tail_cache_ = publishRef().load(std::memory_order_acquire);
      if (rd >= tail_cache_) return {};  // drained
    }
    u64_t const d = descRef(rd).load(std::memory_order_relaxed);
    assert(stateOf(d) == State::kCommitted && "tail advanced over a non-committed record");
    peek_extent_ = extentOf(d);
    return {arena_ + ((rd + kHeaderSize) & mask_), committedLen(d)};
  }

  // Cheap sweep probe, reader thread only. Relaxed on purpose: peek() redoes
  // the load with acquire before any header is trusted.
  [[nodiscard]] bool maybeReadable() const noexcept {
    return rd_ < tail_cache_ ||
           rd_ < std::atomic_ref<u64_t>(sc_->publishPos()).load(std::memory_order_relaxed);
  }

  void pop() noexcept {
    assert(rd_ < tail_cache_ && "pop without a preceding successful peek");
    u64_t const prev = rd_;
    u64_t const extent = peek_extent_ != 0 ? peek_extent_
                                            : extentOf(descRef(rd_).load(std::memory_order_relaxed));
    peek_extent_ = 0;
    rd_ += extent;  // rd_ is reader-private
    readPosRef().store(rd_, std::memory_order_release);             // license to overwrite
    if ((prev ^ rd_) >> shift_) policy_.onWrap(rd_ >> shift_);
  }

  // ---- attachment (cold) -----------------------------------------------------

  // Claims sole ownership of this ring through the Region's shared writer
  // registry (slot index == shard index). The primary claim is ONE wait-free
  // fetch_or on the slot's occupancy bit -- deliberately NOT a compare_exchange
  // retry loop; rewriting it as one would silently downgrade registration from
  // wait-free to lock-free.
  //
  // An occupied slot may be TAKEN OVER, but recycling a dead writer's ring is
  // the one genuine hazard of this variant -- it reintroduces cross-writer byte
  // reuse, which everything else here is shaped to avoid -- so two gates, BOTH
  // required:
  //   (a) the recorded owner is PROVEN dead via liveness.hh, never a timeout.
  //       A stopped-but-alive owner could resume and commit into a ring that
  //       now has a second writer: the exact failure this variant deletes.
  //   (b) the reader has drained the ring to its tail, so the new owner starts
  //       with an empty ring and no byte a peeked span may still reference is
  //       ever rewritten by a different writer. A dead owner's tail is frozen,
  //       so once observed drained the check cannot go stale.
  [[nodiscard, gnu::noinline, gnu::cold]] bool attachWriter() noexcept {
    if (arena_ == nullptr) return false;
    registerAtfork();
    u32_t const tid = currentTid();
    u64_t const prev = bmWordRef().fetch_or(bm_bit_, std::memory_order_acq_rel);
    if ((prev & bm_bit_) == 0) {  // won a free slot
      slotOwnerRef().store(tid, std::memory_order_release);
      bindWriter(tid);
      return true;
    }
    u32_t cur = slotOwnerRef().load(std::memory_order_acquire);
    if (cur == tid) {
      // Only the object that owns the private cursors may reattach. A second
      // view over this live slot would have an independent tail/reservation.
      return wr_tid_ == tid;
    }
    // A freed slot passes through owner == 0 (detachWriter zeroes the owner
    // BEFORE clearing the bit), so a mid-handoff slot is never mistaken for a
    // dead one: takeover requires CAS-ing out a nonzero dead tid.
    if (cur == 0 || threadAlive(cur)) return false;  // gate (a)
    u64_t const tail = publishRef().load(std::memory_order_acquire);
    if (readPosRef().load(std::memory_order_acquire) != tail) return false;  // gate (b)
    // Arbitrating concurrent recyclers by CAS is acceptable HERE: takeover is
    // a cold path, not the wait-free primary claim above.
    if (!slotOwnerRef().compare_exchange_strong(cur, tid, std::memory_order_acq_rel,
                                                std::memory_order_relaxed)) {
      return false;
    }
    // Fence any stale actor still holding the old {slot, generation} pair.
    slotGenRef().fetch_add(1, std::memory_order_acq_rel);
    bindWriter(tid);
    return true;
  }

  [[gnu::noinline, gnu::cold]] void detachWriter() noexcept {
    if (!ownsWriter()) [[unlikely]]
      misuseTrap("SpscRing::detachWriter by a thread that does not own this ring");
    if (res_pos_ != kInvalidPos) abort();  // RAII backstop for misuse (a)
    // Zero the owner BEFORE releasing the bit; see attachWriter.
    slotOwnerRef().store(0, std::memory_order_release);
    bmWordRef().fetch_and(~bm_bit_, std::memory_order_release);
    // Poison this view's private ownership identity. Without this, a stale view
    // can re-attach after another same-tid view claims the slot and both then
    // pass ownsWriter() with independent cursors.
    wr_tid_ = 0;
    // Undrained committed records may remain, and that is fine: they are
    // complete (the tail is a publication cursor), the reader drains them
    // normally, and a successor writer appends after them -- the capacity
    // check keeps it off every undrained byte. Only DEATH needs the drain
    // gate, because a voluntary detach proves by getting here that no
    // reservation is outstanding and every store has been issued.
  }

  // Idempotent for the current holder: Sharded attaches ring by ring, so only
  // the first call performs the queue-wide claim; the rest re-sync cursors.
  [[nodiscard, gnu::noinline, gnu::cold]] bool attachReader() noexcept {
    if (arena_ == nullptr) return false;
    auto rt = readerTidRef();
    u32_t const self = currentTid();
    u32_t cur = rt.load(std::memory_order_acquire);
    if (cur != self) {
      if (cur != 0 && threadAlive(cur)) return false;  // a live reader exists
      if (!rt.compare_exchange_strong(cur, self, std::memory_order_acq_rel,
                                      std::memory_order_relaxed)) {
        return false;  // lost the takeover race
      }
    }
    rd_ = readPosRef().load(std::memory_order_acquire);  // resume exactly here
    tail_cache_ = rd_;  // force the next sweep to re-acquire the tail
    peek_extent_ = 0;
    return true;
  }

  [[nodiscard]] Region const& region() const noexcept { return owned_; }
  [[nodiscard]] Policy& policy() noexcept { return policy_; }

 private:
  void bind(Region const& region, u32_t shard_index) noexcept {
    ctl_ = region.control();
    sc_ = region.shard(shard_index);
    arena_ = region.arena(shard_index);
    cap_ = region.capacity();
    mask_ = region.mask();
    shift_ = static_cast<u32_t>(std::countr_zero(cap_));
    max_need_ = cap_ < kMaxExtent ? cap_ : kMaxExtent;
    // Registry words are inside the mapping, so these stay valid across moves.
    bm_word_ = region.writerBitmap() + shard_index / 64;
    bm_bit_ = 1ull << (shard_index % 64);
    slot_ = region.writerSlots() + shard_index;
  }

  void moveFrom(SpscRing&& other) noexcept {
    owned_ = std::move(other.owned_);
    ctl_ = other.ctl_;
    sc_ = other.sc_;
    arena_ = other.arena_;
    cap_ = other.cap_;
    mask_ = other.mask_;
    max_need_ = other.max_need_;
    shift_ = other.shift_;
    bm_word_ = other.bm_word_;
    bm_bit_ = other.bm_bit_;
    slot_ = other.slot_;
    wr_tail_ = other.wr_tail_;
    wr_read_cache_ = other.wr_read_cache_;
    res_pos_ = other.res_pos_;
    res_need_ = other.res_need_;
    res_n_ = other.res_n_;
    wr_tid_ = other.wr_tid_;
    wr_status_ = other.wr_status_;
    rd_ = other.rd_;
    tail_cache_ = other.tail_cache_;
    peek_extent_ = other.peek_extent_;
    policy_ = std::move(other.policy_);
    other.ctl_ = nullptr;
    other.sc_ = nullptr;
    other.arena_ = nullptr;
    other.slot_ = nullptr;
    other.bm_word_ = nullptr;
    other.res_pos_ = kInvalidPos;
    other.wr_tid_ = 0;
  }

  // Identity only, never an object address: one writer thread may own several
  // SPSC rings, and a moved or reconstructed view must not inherit an address
  // binding. Cleared in a fork child before any inherited private state is used.
  inline static thread_local u32_t writer_tid_tls_ = 0;

  static void atforkChild() noexcept { writer_tid_tls_ = 0; }
  static void registerAtfork() noexcept {
    static std::once_flag once;
    std::call_once(once, [] { ::pthread_atfork(nullptr, nullptr, &atforkChild); });
  }

  [[nodiscard]] bool ownsWriter() const noexcept {
    return writer_tid_tls_ != 0 && wr_tid_ == writer_tid_tls_ &&
           slotOwnerRef().load(std::memory_order_relaxed) == wr_tid_;
  }

  void bindWriter(u32_t tid) noexcept {
    writer_tid_tls_ = tid;
    wr_tid_ = tid;
    wr_tail_ = publishRef().load(std::memory_order_acquire);
    wr_read_cache_ = readPosRef().load(std::memory_order_acquire);
    res_pos_ = kInvalidPos;
    res_n_ = 0;
    wr_status_ = Status::kOk;
  }

  [[nodiscard]] std::atomic_ref<u64_t> descRef(u64_t pos) const noexcept {
    return std::atomic_ref<u64_t>(*reinterpret_cast<u64_t*>(arena_ + (pos & mask_)));
  }
  [[nodiscard]] std::atomic_ref<u64_t> publishRef() const noexcept {
    return std::atomic_ref<u64_t>(sc_->publishPos());
  }
  [[nodiscard]] std::atomic_ref<u64_t> readPosRef() const noexcept {
    return std::atomic_ref<u64_t>(sc_->read_pos);
  }
  [[nodiscard]] std::atomic_ref<u32_t> readerTidRef() const noexcept {
    return std::atomic_ref<u32_t>(ctl_->reader_tid);
  }
  [[nodiscard]] std::atomic_ref<u64_t> bmWordRef() const noexcept {
    return std::atomic_ref<u64_t>(*bm_word_);
  }
  [[nodiscard]] std::atomic_ref<u32_t> slotOwnerRef() const noexcept {
    return std::atomic_ref<u32_t>(slot_->owner_tid);
  }
  [[nodiscard]] std::atomic_ref<u32_t> slotGenRef() const noexcept {
    return std::atomic_ref<u32_t>(slot_->generation);
  }

  // Out of line but NOT gnu::cold: this runs once per ring lap (the read_pos
  // refresh), not once per queue lifetime.
  [[gnu::noinline]] WriteSpan reserveSlow(sz_t n, u64_t need) noexcept {
    wr_read_cache_ = readPosRef().load(std::memory_order_acquire);
    u64_t const tail = wr_tail_;
    if (tail + need - wr_read_cache_ <= cap_) {
      res_pos_ = tail;
      res_need_ = need;
      res_n_ = n;
      wr_status_ = Status::kOk;
      return {arena_ + ((tail + kHeaderSize) & mask_), n};
    }
    policy_.onFull(0);
    // Full is normal backpressure; a DEAD reader will never drain, which is a
    // definite error rather than a retry condition. An unattached reader
    // (tid 0) may yet attach, so it counts as full.
    u32_t const reader = readerTidRef().load(std::memory_order_relaxed);
    wr_status_ = (reader != 0 && !threadAlive(reader)) ? Status::kReaderDead : Status::kFull;
    return {};
  }

  Region owned_;  // set only by create()/attach(); empty for a shard-bound ring
  Control* ctl_ = nullptr;
  ShardControl* sc_ = nullptr;
  std::byte* arena_ = nullptr;
  u64_t cap_ = 0;
  u64_t mask_ = 0;
  u64_t max_need_ = 0;  // min(capacity, kMaxExtent): largest admissible extent
  u32_t shift_ = 0;
  u64_t* bm_word_ = nullptr;  // registry word holding this slot's occupancy bit
  u64_t bm_bit_ = 0;
  WriterSlot* slot_ = nullptr;

  // Writer-owned. Sole writer, so plain members instead of TLS.
  u64_t wr_tail_ = 0;        // private copy of the publication cursor
  u64_t wr_read_cache_ = 0;  // last observed read_pos; may lag, never leads
  u64_t res_pos_ = kInvalidPos;
  u64_t res_need_ = 0;
  sz_t res_n_ = 0;
  u32_t wr_tid_ = 0;
  Status wr_status_ = Status::kOk;

  // Reader-owned.
  u64_t rd_ = 0;          // reader-private cursor; a cache of read_pos
  u64_t tail_cache_ = 0;  // last acquired tail; one acquire per sweep
  u64_t peek_extent_ = 0;  // extent acquired by the successful peek

  [[no_unique_address]] Policy policy_{};
};

// ============================================================================
// SECTION: Sharded -- K rings in one Region, writers assigned to shards
// (owner: impl-spsc)
// ============================================================================
//
// Generic composition: sharding is orthogonal to the ring algorithm, so it is
// written once over any RingT rather than duplicated per variant. Owns the
// Region (one file, one mapping, K arenas) and K ring instances; writers are
// routed to a ring at registration, the reader sweeps all of them.
//
// RingT contract, beyond QueueLike:
//   RingT(Region const&, u32_t shard_index [, Policy&])  non-owning shard bind
//   maybeReadable()                    cheap read-side probe for the sweep
//   attachReader() idempotent for the current holder     (attached ring by
//                                      ring; only the first call claims the
//                                      queue-wide reader role)
//   optional: static constexpr u32_t kMaxWriters         per-shard writer cap
//                                      (absent means unbounded; SpscRing says 1)
//
// Ordering is whatever RingT gives WITHIN a shard; there is never an order
// across shards.

template <typename RingT, typename Policy = DefaultPolicy>
class Sharded {
 public:
  Sharded() = default;
  // Moving is safe only before any attach: writer TLS and the reader bind to
  // this object's address.
  Sharded(Sharded&&) noexcept = default;
  Sharded& operator=(Sharded&&) noexcept = default;
  Sharded(Sharded const&) = delete;
  Sharded& operator=(Sharded const&) = delete;

  [[nodiscard]] static bool create(Config const& cfg, Sharded& out) noexcept {
    Sharded fresh;
    if (!Region::create(cfg, fresh.region_)) return false;
    fresh.init();
    out = std::move(fresh);
    return true;
  }
  [[nodiscard]] static bool attach(int fd, Sharded& out) noexcept {
    Sharded fresh;
    if (!Region::attach(fd, fresh.region_)) return false;
    fresh.init();
    out = std::move(fresh);
    return true;
  }

  // ---- writer ----------------------------------------------------------------

  // The attachment guard is a release-mode TRAP, not an assert (verified by
  // mpsc_xproc_test's fork scenario, which delivers a half-filled record to
  // the reader when this check is compiled out): an unattached thread routes
  // to shard 0 by default, and the worst caller of that shape is a fork()
  // child -- the atfork handler cleared its routing TLS, but a single-writer
  // ring's reservation lives in the ring OBJECT, which the child inherited by
  // copy. Letting the call through commits the parent's in-flight reservation
  // from a second process: silent corruption on a ring whose correctness
  // argument is sole ownership. The sticky-binding cross-check stays
  // debug-only; it guards refactors of this class, not caller misuse.
  [[nodiscard, gnu::always_inline]] inline WriteSpan reserve(sz_t n) noexcept {
    WriterTls const& t = tls_;
    if (t.q != this) [[unlikely]] misuseTrap("Sharded write-side call by an unattached thread");
    assert(t.shard == t.bound_shard && "sticky shard binding violated: migration is forbidden");
    return rings_[t.shard].reserve(n);
  }
  [[gnu::always_inline]] inline void commit(WriteSpan reservation, sz_t actual_n) noexcept {
    WriterTls const& t = tls_;
    if (t.q != this) [[unlikely]] misuseTrap("Sharded write-side call by an unattached thread");
    assert(t.shard == t.bound_shard && "sticky shard binding violated: migration is forbidden");
    rings_[t.shard].commit(reservation, actual_n);
  }
  void abort() noexcept {
    WriterTls const& t = tls_;
    if (t.q != this) [[unlikely]] misuseTrap("Sharded write-side call by an unattached thread");
    assert(t.shard == t.bound_shard && "sticky shard binding violated: migration is forbidden");
    rings_[t.shard].abort();
  }
  bool write(void const* data, sz_t n) noexcept {
    WriterTls const& t = tls_;
    if (t.q != this) [[unlikely]] misuseTrap("Sharded write-side call by an unattached thread");
    assert(t.shard == t.bound_shard && "sticky shard binding violated: migration is forbidden");
    return rings_[t.shard].write(data, n);
  }

  [[nodiscard]] Status status() const noexcept {
    WriterTls const& t = tls_;
    if (t.q != this) return t.status;  // not attached: the attach failure reason
    return rings_[t.shard].status();
  }

  // Writer capacity. Exclusive-registration ring types (SpscRing) cap the queue
  // at one writer per shard; everything else is unbounded (api.hh).
  [[nodiscard]] u32_t maxWriters() const noexcept {
    if constexpr (kMaxWritersPerShard == kUnboundedWriters) {
      return kUnboundedWriters;
    } else {
      u64_t const cap = static_cast<u64_t>(kMaxWritersPerShard) * shard_count_;
      return cap >= kUnboundedWriters ? kUnboundedWriters : static_cast<u32_t>(cap);
    }
  }

  // ---- reader ----------------------------------------------------------------

  // Sweeps the shards. The probe keeps the sweep cheap (no acquire, no
  // liveness) on shards with nothing deliverable; a probe-true shard may still
  // peek empty (an in-flight record in an ordered ring), which is why the scan
  // continues rather than spins.
  [[nodiscard]] ReadSpan peek() noexcept {
    u32_t idx = sweep_start_;
    for (u32_t k = 0; k < shard_count_; ++k) {
      u32_t const i = idx;
      idx = idx + 1 == shard_count_ ? 0 : idx + 1;
      if (!rings_[i].maybeReadable()) continue;
      ReadSpan const s = rings_[i].peek();
      if (s.data() != nullptr) {  // data(), not empty(): records may be 0-length
        cur_ = i;
        empty_iters_ = 0;
        return s;
      }
    }
    // Rotate even on an empty sweep, so a probe-true-but-empty shard (a stalled
    // in-flight record) cannot pin the scan order against later shards.
    sweep_start_ = sweep_start_ + 1 == shard_count_ ? 0 : sweep_start_ + 1;
    policy_.onEmpty(empty_iters_++);
    return {};
  }

  void pop() noexcept {
    rings_[cur_].pop();
    // Rotate the next sweep past the shard just served so no shard starves.
    sweep_start_ = cur_ + 1 == shard_count_ ? 0 : cur_ + 1;
  }

  // ---- attachment (cold) -----------------------------------------------------

  [[nodiscard, gnu::noinline, gnu::cold]] bool attachWriter() noexcept {
    WriterTls& t = tls_;
    // Idempotent for an already-bound thread, KEEPING its shard: assignment is
    // sticky (see bindTls), so a repeat attach must never re-route -- in a
    // release build a re-run of placement here would BE the forbidden
    // migration.
    if (t.q == this) return true;
    if (t.q != nullptr) [[unlikely]]
      misuseTrap("Sharded::attachWriter while attached to another queue");
    registerAtfork();
    if constexpr (kMaxWritersPerShard == 1) {
      // Single-writer rings arbitrate ownership themselves through the shared
      // registry (wait-free bit claim; takeover gated on proven death and a
      // drained ring). Occupancy per shard is 0-or-1, so first-free IS
      // least-occupied and no count table is involved.
      for (u32_t i = 0; i < shard_count_; ++i) {
        if (rings_[i].attachWriter()) return bindTls(t, i);
      }
    } else {
      // LEAST-OCCUPIED placement, deliberately NOT hash(tid). A static hash
      // locks a hot writer to whoever it collided with forever: balls-in-bins
      // at 1000 writers over 64 shards puts the worst shard near 28-30 writers
      // instead of the advertised ~16, and at small counts the birthday bound
      // is brutal -- two writers collide with ~54% probability at 10 writers
      // over 64 shards, exactly the deployment where each writer is likely
      // hot. Registration is cold, so the scan costs nothing. Do not
      // "simplify" this back to a hash.
      //
      // SCOPE OF THE CLAIM: the count table is process-local, so
      // "least-occupied" holds WITHIN a process only. Writers in separate
      // processes each balance against their own view, can independently pick
      // the same "emptiest" shard, and the cross-process balance degrades
      // toward random -- still strictly no worse than hash(tid), which is
      // random placement with permanent collisions. Correctness never depends
      // on placement (multi-writer rings admit any assignment); restoring the
      // property cross-process means promoting the counts into the shared
      // registry area next to the writer slots, with updates kept on this cold
      // path only. Placement can only ever be improved HERE, at registration:
      // assignment is sticky (see bindTls) because per-writer FIFO depends on
      // it, so migrating live writers is never an available lever.
      for (u32_t attempts = 0; attempts <= shard_count_; ++attempts) {
        u32_t best = kNoShard;
        u32_t best_count = kMaxWritersPerShard;
        for (u32_t i = 0; i < shard_count_; ++i) {
          u32_t const c = counts_[i].load(std::memory_order_relaxed);
          if (c < best_count) {
            best_count = c;
            best = i;
          }
        }
        if (best == kNoShard) break;  // every shard at its writer limit
        u32_t expected = best_count;
        if (!counts_[best].compare_exchange_strong(
              expected, best_count + 1, std::memory_order_acq_rel, std::memory_order_relaxed)) {
          continue;  // raced another attacher; rescan
        }
        if (rings_[best].attachWriter()) return bindTls(t, best);
        counts_[best].fetch_sub(1, std::memory_order_acq_rel);
      }
    }
    t.status = Status::kNoSlot;
    return false;
  }

  [[gnu::noinline, gnu::cold]] void detachWriter() noexcept {
    WriterTls& t = tls_;
    // Trap, not assert: an unattached thread would detach shard 0's writer --
    // releasing a slot some OTHER live writer owns, which a later attach then
    // double-claims. Corruption-class, so it stays on in release.
    if (t.q != this) [[unlikely]] misuseTrap("Sharded::detachWriter by an unattached thread");
    assert(t.shard == t.bound_shard && "sticky shard binding violated: migration is forbidden");
    // Detach ends this writer's FIFO epoch. A later re-registration may land
    // on a different shard, and ordering ACROSS registrations is not promised:
    // undrained records from the old shard and new records interleave at the
    // reader's discretion. Within one registration, stickiness makes the
    // stream FIFO.
    rings_[t.shard].detachWriter();
    if constexpr (kMaxWritersPerShard != 1) {
      counts_[t.shard].fetch_sub(1, std::memory_order_acq_rel);
    }
    t.q = nullptr;
  }

  [[nodiscard, gnu::noinline, gnu::cold]] bool attachReader() noexcept {
    // Ring 0 claims (or takes over) the queue-wide reader role in the control
    // page; the remaining calls are the same thread re-attaching, which the
    // RingT contract requires to be an idempotent cursor re-sync.
    for (u32_t i = 0; i < shard_count_; ++i) {
      if (!rings_[i].attachReader()) return false;
    }
    sweep_start_ = 0;
    cur_ = 0;
    empty_iters_ = 0;
    return true;
  }

  [[nodiscard]] Region const& region() const noexcept { return region_; }
  [[nodiscard]] Policy& policy() noexcept { return policy_; }

 private:
  static constexpr u32_t kNoShard = ~0u;
  static constexpr u32_t kMaxWritersPerShard = [] {
    if constexpr (requires { RingT::kMaxWriters; }) {
      return RingT::kMaxWriters;
    } else {
      return ~0u;  // unbounded: the ring arbitrates (or admits) writers itself
    }
  }();

  // Per-thread shard binding. One binding per thread, same discipline as the
  // rings' own writer state.
  struct WriterTls {
    Sharded const* q = nullptr;
    u32_t shard = 0;
    // Debug tripwire for the sticky-binding guarantee: written ONLY by
    // bindTls, cross-checked against `shard` on every write-side call. A
    // future refactor that re-routes `shard` anywhere else trips the assert
    // instead of silently reordering a writer's stream.
    u32_t bound_shard = 0;
    Status status = Status::kOk;
  };
  inline static thread_local WriterTls tls_{};

  // Fork invariant (stated on Ring::registerAtfork): after fork a child holds
  // NO reservation and NO writer slot, and must re-register before writing.
  // Each layer registers its OWN atfork child handler for its own thread-local
  // state -- this one clears only Sharded's shard routing, so a forked child
  // cannot reserve() into the parent's shard (for SpscRing, a second writer on
  // a sole-owner ring: silent corruption); it must win its own slot from the
  // shared registry first. The rings' reservation TLS is theirs to clear.
  static void atforkChild() noexcept { tls_ = WriterTls{}; }
  static void registerAtfork() noexcept {
    static std::once_flag once;
    std::call_once(once, [] { ::pthread_atfork(nullptr, nullptr, &atforkChild); });
  }

  // THE binding site, and the binding is STICKY: a writer is assigned exactly
  // one shard at registration and keeps it for the lifetime of the
  // registration. This is load-bearing for per-writer FIFO -- all of a
  // writer's records land in one ring, so they are ordered with respect to
  // each other. Migrating a live writer to another shard would let the reader
  // sweep deliver its later records before its earlier ones: silent reordering
  // no caller can detect. DO NOT add migration or rebalancing here; placement
  // quality is a registration-time concern only (see attachWriter).
  bool bindTls(WriterTls& t, u32_t shard) noexcept {
    t.q = this;
    t.shard = shard;
    t.bound_shard = shard;  // the only store to bound_shard; see WriterTls
    t.status = Status::kOk;
    return true;
  }

  void init() noexcept {
    rings_.clear();
    counts_.reset();
    shard_count_ = region_.shardCount();
    sweep_start_ = 0;
    cur_ = 0;
    empty_iters_ = 0;
    // The only allocations, made once at construction: ring views and the
    // placement count table.
    rings_.reserve(shard_count_);
    for (u32_t i = 0; i < shard_count_; ++i) {
      if constexpr (std::is_constructible_v<RingT, Region const&, u32_t, Policy&>) {
        rings_.emplace_back(region_, i, policy_);  // policies are handles; copies share state
      } else {
        rings_.emplace_back(region_, i);
      }
    }
    counts_ = std::make_unique<std::atomic<u32_t>[]>(shard_count_);
  }

  Region region_;
  std::vector<RingT> rings_;
  std::unique_ptr<std::atomic<u32_t>[]> counts_;  // writers per shard (placement only)
  u32_t shard_count_ = 0;
  u32_t sweep_start_ = 0;  // reader-private rotation cursor
  u32_t cur_ = 0;          // shard the last peek() delivered from
  u32_t empty_iters_ = 0;
  [[no_unique_address]] Policy policy_{};
};

// ============================================================================
// SECTION: public aliases
// ============================================================================

using Mpsc = Ring<>;                    // one shared ring, total order
using Spsc = SpscRing<>;                // one ring, one writer, wait-free
using ShardedMpsc = Sharded<Ring<>>;    // K shared rings, per-shard total order
using MultiSpsc = Sharded<SpscRing<>>;  // K SPSC rings, one per writer, wait-free

}  // namespace pgt::mpsc
