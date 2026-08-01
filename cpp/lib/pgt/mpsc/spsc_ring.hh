#pragma once

// The single-producer ring. Its per-position slot-length plane keeps metadata
// out of the mirrored payload arena. Multi-producer work is served by MpscRing
// (mpsc_ring.hh), which additionally separates arbitration from publication.

#include "pgt/core/types.hh"
#include "pgt/mpsc/api.hh"
#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/detail.hh"
#include "pgt/mpsc/liveness.hh"
#include "pgt/mpsc/policy.hh"
#include "pgt/mpsc/region.hh"

#include <pthread.h>
#include <unistd.h>

#include <atomic>
#include <bit>
#include <cassert>
#include <cerrno>
#include <cstring>
#include <mutex>
#include <utility>

namespace pgt::mpsc {

// ============================================================================
// SECTION: SpscRing -- one writer per ring, no claim arbitration (owner:
// impl-spsc)
// ============================================================================
//
// The degenerate case of in-band queue in which claim arbitration, and with it most of
// the protocol, disappears. The load-bearing change: the tail
// (ShardControl::publishPos()) is a PUBLICATION cursor, not a reservation
// cursor -- it advances only at commit, so the reader consumes [read_pos, tail)
// and everything in that range is complete by definition. What that deletes:
//
//   claim CAS           the sole writer's private tail IS the frontier, so
//                       reserve() is a bounds check plus writer-private
//                       bookkeeping, touching NOTHING shared. WAIT-FREE,
//                       strictly stronger than an MPSC ring's lock-free claim.
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
//                       an unfinished reservation has published nothing
//                       (slot store and tail advance both happen at commit),
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
// cached tail still covers unread records -- and reads slot lengths below it
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
  // One writer per ring.
  static constexpr u32_t kMaxWriters = 1;
  static constexpr u64_t kSlotStride = sizeof(u64_t);
  static constexpr u64_t kSlotGrain = kGrain;
  static constexpr sz_t kMaxPayload = static_cast<sz_t>(kMaxExtent);

  [[nodiscard]] static constexpr sz_t extentForPayload(sz_t payload) noexcept {
    sz_t const extent = (payload + (kSlotGrain - 1)) & ~(kSlotGrain - 1);
    return extent == 0 ? kSlotGrain : extent;
  }

  [[nodiscard]] static constexpr bool extentForPayloadChecked(sz_t payload, u64_t& out) noexcept {
    if (payload > kMaxPayload) return false;
    out = extentForPayload(payload);
    return out <= kMaxExtent;
  }

  SpscRing() = default;

  // Non-owning shard bind, as an owning or non-owning view: the Region must outlive the ring.
  SpscRing(Region const& region, u32_t shard_index, Policy& policy) noexcept : policy_(policy) {
    bind(region, shard_index);
  }
  SpscRing(Region const& region, u32_t shard_index) noexcept { bind(region, shard_index); }

  SpscRing(SpscRing&& other) noexcept { moveFrom(std::move(other)); }
  SpscRing& operator=(SpscRing&& other) noexcept {
    if (this != &other) {
      if (ownsWriter()) detail::misuseTrap("moving over an attached SpscRing");
      if (reader_attached_) detail::misuseTrap("moving over an attached SpscRing reader");
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
    c.plane_claim_stride = kSlotStride;
    c.plane_result_stride = 0;
    c.plane_grain = kSlotGrain;
    if (!Region::create(c, out.owned_)) return false;
    out.bind(out.owned_, 0);
    return true;
  }
  [[nodiscard]] static bool attach(int fd, SpscRing& out) noexcept {
    int const dup_fd = ::dup(fd);
    if (dup_fd < 0) return false;
    Region r;
    if (!Region::attach(dup_fd, r)) {
      int const saved = errno;
      ::close(dup_fd);
      errno = saved;
      return false;
    }
    if (r.shardCount() != 1 || r.claimStride() != kSlotStride || r.resultStride() != 0 ||
        r.planeGrain() != kSlotGrain) {
      errno = EPROTO;
      return false;  // r closes dup_fd; the caller's fd remains untouched
    }
    out.owned_ = std::move(r);
    out.bind(out.owned_, 0);
    return true;
  }

  // ---- writer ----------------------------------------------------------------

  // WAIT-FREE: a bounds check against the cached read position and
  // writer-private bookkeeping. No shared load or store on the fast path --
  // the ring is not touched until commit().
  [[nodiscard, gnu::always_inline]] inline WriteSpan reserve(sz_t n) noexcept {
    assert(res_pos_ == detail::kInvalidPos && "reserve while a reservation is held");  // misuse (d)
    if (!ownsWriter()) [[unlikely]]
      detail::misuseTrap("SpscRing::reserve by a thread that does not own this ring");
    // Representability first, THEN capacity. Rounding an unrepresentable length
    // can overflow to a small-looking extent, and zero would make a reader stop
    // advancing.
    u64_t need = 0;
    if (!extentForPayloadChecked(n, need) || need > max_need_) [[unlikely]] {
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
    return {arena_ + (tail & mask_), n};
  }

  // Misuse traps, ON in release (the queue doctrine): each failure mode is silent
  // cross-process corruption -- a double commit republishes a stale
  // reservation, a grown commit publishes a fictitious boundary -- and each
  // check is a predictable never-taken branch over writer-private state.
  [[gnu::always_inline]] inline void commit(WriteSpan reservation, sz_t actual_n) noexcept {
    if (!ownsWriter()) [[unlikely]]
      detail::misuseTrap("SpscRing::commit by a thread that does not own this ring");
    if (res_pos_ == detail::kInvalidPos) [[unlikely]]
      detail::misuseTrap("SpscRing::commit without a live reservation");  // misuse (b)
    if (reservation.data() != arena_ + (res_pos_ & mask_) || reservation.size() != res_n_)
      detail::misuseTrap("SpscRing::commit with a span other than the live reservation");
    if (actual_n > res_n_) [[unlikely]]
      detail::misuseTrap("SpscRing::committed payload exceeds the reservation");
    // Safe without a checked form: actual_n <= res_n_ was just enforced, and
    // res_n_ passed extentForPayloadChecked() at reserve().
    u64_t const used = extentForPayload(actual_n);
    if (used > res_need_) [[unlikely]]
      detail::misuseTrap("SpscRing::commit extent exceeds the reservation");  // misuse (c)
    u64_t const p = res_pos_;
    // Relaxed: the reader cannot reach this slot until the tail release below
    // publishes it together with the headerless payload.
    slotLenRef(p).store(actual_n, std::memory_order_relaxed);
    wr_tail_ = p + used;
    publishRef().store(wr_tail_, std::memory_order_release);  // publication
    res_pos_ = detail::kInvalidPos;                           // poison
    policy_.onCommit(p, used);
    // A short commit needs no trailer: the unused remainder was never
    // published, so it returns to this writer's private free space instead of
    // becoming a dead record the reader must skip.
  }

  void abort() noexcept {
    if (!ownsWriter()) [[unlikely]]
      detail::misuseTrap("SpscRing::abort by a thread that does not own this ring");
    if (res_pos_ == detail::kInvalidPos) [[unlikely]]
      detail::misuseTrap("SpscRing::abort without a live reservation");
    policy_.onAbort(res_pos_, res_need_);
    // Nothing was published -- the tail never moved -- so aborting is purely
    // writer-local. No kAborted record exists for the reader to skip.
    res_pos_ = detail::kInvalidPos;  // poison
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
      // The ONE acquire of the sweep: its pairing release covers every slot
      // and payload store at or below the tail, so records under the cached
      // tail are read relaxed until the cache is exhausted.
      tail_cache_ = publishRef().load(std::memory_order_acquire);
      if (rd >= tail_cache_) return {};  // drained
    }
    u64_t const n = slotLenRef(rd).load(std::memory_order_relaxed);
    assert(n <= kMaxPayload && "published SPSC slot length is not representable");
    peek_extent_ = extentForPayload(static_cast<sz_t>(n));
    return {arena_ + (rd & mask_), static_cast<sz_t>(n)};
  }

  // Cheap sweep probe, reader thread only. Relaxed on purpose: peek() redoes
  // the tail load with acquire before any slot length is trusted.
  [[nodiscard]] bool maybeReadable() const noexcept {
    return rd_ < tail_cache_ ||
           rd_ < std::atomic_ref<u64_t>(sc_->publishPos()).load(std::memory_order_relaxed);
  }

  void pop() noexcept {
    assert(rd_ < tail_cache_ && "pop without a preceding successful peek");
    u64_t const prev = rd_;
    assert(peek_extent_ != 0 && "pop without a preceding successful peek");
    u64_t const extent = peek_extent_;
    peek_extent_ = 0;
    rd_ += extent;                                       // rd_ is reader-private
    readPosRef().store(rd_, std::memory_order_release);  // license to overwrite
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
      detail::misuseTrap("SpscRing::detachWriter by a thread that does not own this ring");
    if (res_pos_ != detail::kInvalidPos) abort();  // RAII backstop for misuse (a)
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

  // Idempotent only for this view. The shared reader slot stores a kernel TID,
  // not a view identity, so a same-thread second view would otherwise see
  // `reader_tid == self` and split the stream with an independent rd_. That is
  // refused locally; a same-object call keeps the existing ownership and merely
  // re-syncs cursors. Exact cross-object protection after kernel TID reuse is
  // outside this ABI: the shared slot has no start-time/token field.
  [[nodiscard, gnu::noinline, gnu::cold]] bool attachReader() noexcept {
    if (arena_ == nullptr) return false;
    auto rt = readerTidRef();
    u32_t const self = currentTid();
    u32_t cur = rt.load(std::memory_order_acquire);
    if (cur == self && !reader_attached_) return false;
    if (cur != self) {
      if (cur != 0 && threadAlive(cur)) return false;  // a live reader exists
      if (!rt.compare_exchange_strong(cur, self, std::memory_order_acq_rel,
                                      std::memory_order_relaxed)) {
        return false;  // lost the takeover race
      }
    }
    reader_attached_ = true;
    rd_ = readPosRef().load(std::memory_order_acquire);  // resume exactly here
    tail_cache_ = rd_;  // force the next sweep to re-acquire the tail
    peek_extent_ = 0;
    return true;
  }

  // Reader ownership is explicit. Destruction does not silently detach: that
  // keeps crash semantics honest, leaving successors to prove TID death before
  // replaying from read_pos. A live handoff must call detachReader().
  [[gnu::noinline, gnu::cold]] void detachReader() noexcept {
    if (!reader_attached_) [[unlikely]]
      detail::misuseTrap("SpscRing::detachReader by a view that is not attached");
    u32_t expected = currentTid();
    if (!readerTidRef().compare_exchange_strong(expected, 0, std::memory_order_acq_rel,
                                                std::memory_order_relaxed)) {
      detail::misuseTrap("SpscRing::detachReader by a thread that does not own the reader slot");
    }
    reader_attached_ = false;
    peek_extent_ = 0;
  }

  [[nodiscard]] Region const& region() const noexcept { return owned_; }
  [[nodiscard]] Policy& policy() noexcept { return policy_; }

 private:
  void bind(Region const& region, u32_t shard_index) noexcept {
    if (!region.valid() || shard_index >= region.shardCount() ||
        region.claimStride() != kSlotStride || region.resultStride() != 0 ||
        region.planeGrain() != kSlotGrain || region.claimPlane(shard_index) == nullptr) {
      detail::misuseTrap("binding SpscRing to incompatible Region geometry");
    }
    ctl_ = region.control();
    sc_ = region.shard(shard_index);
    arena_ = region.arena(shard_index);
    slot_plane_ = region.claimPlane(shard_index);
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
    slot_plane_ = other.slot_plane_;
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
    wr_gen_ = other.wr_gen_;
    wr_status_ = other.wr_status_;
    rd_ = other.rd_;
    tail_cache_ = other.tail_cache_;
    peek_extent_ = other.peek_extent_;
    reader_attached_ = other.reader_attached_;
    policy_ = std::move(other.policy_);
    other.ctl_ = nullptr;
    other.sc_ = nullptr;
    other.arena_ = nullptr;
    other.slot_plane_ = nullptr;
    other.slot_ = nullptr;
    other.bm_word_ = nullptr;
    other.res_pos_ = detail::kInvalidPos;
    other.wr_tid_ = 0;
    other.wr_gen_ = 0;
    other.reader_attached_ = false;
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

  // Ownership is {tid, slot owner, slot GENERATION}. The generation is what
  // makes takeover fencing real: it was previously bumped on takeover and read
  // by nobody, so the comment there promised a fence that did not exist.
  //
  // The second load is free -- WriterSlot is 8 bytes and owner and generation
  // share the line the owner check already pulled in.
  [[nodiscard]] bool ownsWriter() const noexcept {
    return writer_tid_tls_ != 0 && wr_tid_ == writer_tid_tls_ &&
           slotOwnerRef().load(std::memory_order_relaxed) == wr_tid_ &&
           slotGenRef().load(std::memory_order_relaxed) == wr_gen_;
  }

  void bindWriter(u32_t tid) noexcept {
    writer_tid_tls_ = tid;
    wr_tid_ = tid;
    // Capture AFTER any takeover bump, so this view is bound to the incarnation
    // it actually won rather than the one it displaced.
    wr_gen_ = slotGenRef().load(std::memory_order_acquire);
    wr_tail_ = publishRef().load(std::memory_order_acquire);
    wr_read_cache_ = readPosRef().load(std::memory_order_acquire);
    res_pos_ = detail::kInvalidPos;
    res_n_ = 0;
    wr_status_ = Status::kOk;
  }

  [[nodiscard]] std::atomic_ref<u64_t> slotLenRef(u64_t pos) const noexcept {
    u64_t const cell = (pos / kSlotGrain) & ((cap_ / kSlotGrain) - 1);
    return std::atomic_ref<u64_t>(*reinterpret_cast<u64_t*>(slot_plane_ + cell * kSlotStride));
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
      return {arena_ + (tail & mask_), n};
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
  std::byte* slot_plane_ = nullptr;
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
  u64_t res_pos_ = detail::kInvalidPos;
  u64_t res_need_ = 0;
  sz_t res_n_ = 0;
  u32_t wr_tid_ = 0;
  u32_t wr_gen_ = 0;  // slot incarnation this view bound against
  Status wr_status_ = Status::kOk;

  // Reader-owned.
  u64_t rd_ = 0;           // reader-private cursor; a cache of read_pos
  u64_t tail_cache_ = 0;   // last acquired tail; one acquire per sweep
  u64_t peek_extent_ = 0;  // extent acquired by the successful peek
  bool reader_attached_ = false;

  [[no_unique_address]] Policy policy_{};
};

static_assert(QueueLike<SpscRing<DefaultPolicy>>);
static_assert(std::atomic_ref<u32_t>::is_always_lock_free);
static_assert(std::atomic_ref<u64_t>::is_always_lock_free);

}  // namespace pgt::mpsc
