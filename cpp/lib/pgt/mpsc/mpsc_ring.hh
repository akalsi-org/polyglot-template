#pragma once

// Many-producer, single-consumer mirrored ring. MpscRing separates claim,
// publication, and payload storage; it replaces the removed in-band MPSC queue.
//
// ============================================================================
// Why
// ============================================================================
//
// `in-band queue` collapses three unrelated jobs onto one cache line. A record's 8-byte
// in-band descriptor shares its line with the first 56 bytes of its own
// payload, so:
//
//   * the reader polling for a commit downgrades M->S the very line the owner
//     is actively memcpy-ing into;
//   * claim contenders race a CAS on the same line as that payload;
//   * the successor stamp lands on a COLD payload line one extent ahead.
//
// The two-plane layout gives each job its own memory:
//
//   Claim[M]    arbitration and recoverable ownership. 8 bytes per 64-byte
//               payload grain. Claimed by exact CAS from a self-certifying
//               FREE(p) word -- the SAME encoding `in-band queue` uses, unchanged.
//   Result[M]   a SEPARATE array (not interleaved with Claim). 16 bytes per
//               grain: {committed length, self-certifying commit tag}. The
//               reader polls here; the owner writes it once, after the payload
//               is complete.
//   payload     the unchanged mirrored arena, but with NO in-band descriptor,
//               NO successor stamp inside it, NO short-commit trailer.
//
// ============================================================================
// The slot-count bound needs no accounting of its own
// ============================================================================
//
// M = capacity / 64. Every record occupies at least one 64-byte grain, so the
// number of simultaneously live records can never exceed M. Both planes are
// indexed by GRAIN, not by record ordinal:
//
//     cell(p) = (p >> 6) & (M - 1)
//
// That single choice removes most of the complexity a record-ordinal wheel
// would need. There is no ordinal to carry, no {ordinal, byte-position} pair to
// publish atomically, no separate slot credit, no slot full-check, and no
// reader retirement journal: the byte-capacity check remains the sole admission
// gate, and the reader's whole published state stays the single `read_pos` word
// -- so reader restart still needs nothing new.
//
// Lap reuse is defeated exactly as `in-band queue` defeats it: the FREE word carries the
// unwrapped position (`freeWord`, desc.hh), so FREE(p) != FREE(p + kN), and the
// commit tag carries the same unwrapped position.
//
// ============================================================================
// What changes relative to in-band queue, and what does not
// ============================================================================
//
// DELETED
//   * the in-band descriptor: payload starts at p, not p + 8. Records whose
//     length is a multiple of 64 now pack exactly instead of costing a whole
//     extra grain.
//   * the short-commit trailer. The Claim word keeps the FULL reserved extent
//     forever; Result carries the actual length. The reader delivers n bytes
//     and advances by the reserved extent -- which is what `in-band queue`'s trailer
//     amounted to anyway, since a trailer never freed the suffix early.
//   * the walker's "kClaimed blocks the walk" rule. See below; this is the
//     change with the most upside.
//
// KEPT, DELIBERATELY
//   * the exact `freeWord(p)` claim CAS, and with it Lemma 1: a successful CAS
//     proves by content alone that p was the frontier, however the walk got
//     there. This is why the reader must NOT pre-stamp free cells ahead of the
//     frontier -- doing so would make FREE(p) common instead of unique and
//     would trade Lemma 1 for a much weaker "the hint never leads" argument.
//     The frontier is created by the PREDECESSOR's promotion store, exactly as
//     in `in-band queue`, so at most one reachable cell reads FREE for its own position.
//   * the kCleared vouch. It has exactly one remaining consumer: reader
//     recovery, which must know whether a dead owner got as far as promoting
//     its successor cell. It is now a second store to the line the claim CAS
//     just took exclusively, instead of `in-band queue`'s store to a cold payload line.
//   * reader-only recovery, gated on proven death via /proc. No timeouts.
//   * the +kGrain admission slack, so effective capacity is directly
//     comparable with `in-band queue` in a benchmark.
//
// WALKING PAST AN IN-FLIGHT RECORD IS NOW LEGAL. In `in-band queue` a walker meeting
// kClaimed must restart, because the successor slot may still hold stale
// PAYLOAD -- arbitrary bytes that can decode as anything. Here the successor
// cell is a Claim cell, and the only values it can hold are well-formed claim
// words, this lap's or an earlier lap's. A walker advancing on a stale word
// wanders, but it can never make a bad claim: only the true frontier cell reads
// exactly freeWord(p) for the p the walker holds. Wandering is bounded by the
// hop cap and ends in a restart. So a live writer holding a reservation across
// user code no longer stalls every other writer's walk -- only the reader.
//
// ============================================================================
// Honest cost accounting
// ============================================================================
//
// Metadata is 8 (Claim) + 16 (Result) = 24 bytes per 64 bytes of payload ring,
// i.e. 37.5% of payload capacity -- not the ~50% estimated before the layout
// was pinned down. Padding each cell to its own cache line (kPadded) raises
// that to 200%.
//
// Against that, deleting the in-band header gives capacity back for payloads
// that are multiples of 64: a 64-byte payload costs 128 bytes of ring in `in-band queue`
// and 64 + 24 = 88 here.
//
// ============================================================================
// Memory ordering
// ============================================================================
//
//   claim CAS Claim[cell(p)] FREE(p)->CLAIMED   acquire success / relaxed fail
//   successor promotion Claim[cell(q)]=FREE(q)  relaxed  (exclusive; no payload)
//   vouch Claim[cell(p)]=CLEARED                release  (publishes the promotion)
//   write_hint store / load                     release / acquire
//   Result.len store                            relaxed  (published by the tag)
//   Result.tag store                            release  (publishes the payload)
//   Result.tag load (reader)                    acquire
//   Claim walk / reader load                    acquire
//   recovery promotion                          relaxed
//   recovery Claim[cell(p)]=ABORTED             release
//   read_pos store / load                       release / acquire
//
// ============================================================================
// Design limitations, stated rather than implied
// ============================================================================
//
//   * sharding composition is not wired up; this is a standalone ring only.
//   * The executable model at docs/verification/queue-model.md is bounded
//     exhaustive evidence, not an unbounded proof. The older Spin model was
//     written against the removed in-band ring and is retained, marked as such,
//     at docs/verification/removed-inband-ring.pml.

#include "pgt/core/types.hh"
#include "pgt/mpsc/api.hh"
#include "pgt/mpsc/desc.hh"
#include "pgt/mpsc/liveness.hh"
#include "pgt/mpsc/policy.hh"
#include "pgt/mpsc/detail.hh"
#include "pgt/mpsc/region.hh"

#include <pthread.h>
#include <sys/mman.h>

#include <atomic>
#include <bit>
#include <cassert>
#include <cstring>
#include <mutex>
#include <span>
#include <utility>

namespace pgt::mpsc {

// ---------------------------------------------------------------------------
// Result-cell encoding
// ---------------------------------------------------------------------------
//
// Sixteen bytes: {len, tag}. The TAG is the published word and it is
// self-certifying in exactly the way freeWord() is -- it carries the unwrapped
// position of the record it belongs to, so a stale tag left in the cell by any
// earlier lap is rejected by content alone. Bit 0 is always set, so a
// zero-filled cell (the initial state, and the state of every never-committed
// cell) is never a valid tag.
//
// Making the tag exact rather than a truncated generation is what lets the
// reader write NOTHING to either plane on the normal path: it never has to
// scrub Result cells, so its only shared store per record batch remains
// `read_pos`. It also keeps reader restart free -- a successor reader resumes
// from `read_pos` and every cell it then meets is still self-describing.
struct alignas(16) ResultCell {
  u64_t len;
  u64_t tag;
};

[[nodiscard]] inline constexpr u64_t commitTag(u64_t pos) noexcept {
  return (((pos >> 6) & kFreePosMask) << 1) | 1ull;
}

// State value used only by this variant, in the same low-3-bit field desc.hh
// pins. kFree/kClaimed/kCleared/kAborted keep their desc.hh meanings and their
// ABI values; nothing here reinterprets an existing encoding.
inline constexpr u64_t kTpFree = static_cast<u64_t>(State::kFree);
inline constexpr u64_t kTpClaimed = static_cast<u64_t>(State::kClaimed);
inline constexpr u64_t kTpCleared = static_cast<u64_t>(State::kCleared);
inline constexpr u64_t kTpAborted = static_cast<u64_t>(State::kAborted);

// Reservation extent for the two-plane layout: no 8-byte in-band header, but
// still never zero -- a zero-extent record would let a walker advance by zero
// and would break the "at most M live records" bound the slot planes rest on.
// Precondition: payload <= kTpMaxPayload. The rounding overflows silently
// otherwise, and it fails in the most dangerous possible direction: a payload in
// [SIZE_MAX-62, SIZE_MAX] wraps to 0, the `need == 0` branch below then rewrites
// it as a single grain, and admission -- which only ever sees the extent --
// cheerfully accepts it as the smallest possible record.
//
// Measured before this bound existed: reserve(SIZE_MAX) returned kOk with a
// WriteSpan advertising SIZE_MAX writable bytes over a one-grain reservation.
// write() memcpy's span.size() bytes, so that is an unbounded out-of-bounds
// write reachable from any caller whose length came from an upstream
// subtraction that underflowed.
inline constexpr sz_t kTpMaxPayload = static_cast<sz_t>(kMaxExtent);

template <sz_t CellGrain = kGrain>
[[nodiscard]] inline constexpr sz_t tpExtentFor(sz_t payload) noexcept {
  static_assert(CellGrain >= kGrain && std::has_single_bit(CellGrain));
  sz_t const need = (payload + (CellGrain - 1)) & ~static_cast<sz_t>(CellGrain - 1);
  return need == 0 ? CellGrain : need;
}

// The admission-safe form. Compare against a precomputed bound rather than
// adding, so the check itself cannot overflow.
template <sz_t CellGrain = kGrain>
[[nodiscard]] inline constexpr bool tpExtentForChecked(sz_t payload, u64_t& out) noexcept {
  if (payload > kTpMaxPayload) return false;
  out = tpExtentFor<CellGrain>(payload);
  return out <= kMaxExtent;
}

// The Claim and Result planes are NOT owned here: they live in the Region's
// control area, sized by Config::plane_claim_stride / plane_result_stride. That
// is what makes this variant attachable from an unrelated process -- an earlier
// prototype kept them in a private MAP_SHARED|MAP_ANONYMOUS mapping, which only
// fork() could inherit.
//
// Each plane block is rounded to the 64-byte line, so the padded stride really
// does land one cell per line and the two arrays can never share one.
//
// They must stay in the control area specifically, which is mapped ONCE. A
// mirrored arena maps the same pages at two addresses, and those are distinct
// locations to the compiler, so it may reorder or coalesce relaxed accesses
// that in fact touch one physical word -- a miscompilation hazard that no
// amount of correct atomics can repair.

// `Padded` gives every Claim cell and every Result cell its own cache line.
// Compact packs 8 Claim cells / 4 Result cells per line, which is denser and
// prefetch-friendly but lets neighbouring records' claim CASes and commit
// stores false-share. Which wins is a measurement, not a deduction -- both are
// built and both are benchmarked.
template <typename Policy = DefaultPolicy, bool Padded = false, sz_t CellGrain = kGrain>
class MpscRing {
 public:
  static_assert(CellGrain >= kGrain && std::has_single_bit(CellGrain));
  static_assert(CellGrain <= kMaxExtent);
  static_assert(Padded || CellGrain == kGrain,
                "larger MPSC cells are only supported by the padded layout");
  static constexpr u64_t kCellGrain = CellGrain;
  static constexpr u64_t kClaimStride = Padded ? kCacheLine : sizeof(u64_t);
  static constexpr u64_t kResultStride = Padded ? kCacheLine : sizeof(ResultCell);

  MpscRing() = default;
  explicit MpscRing(Policy policy) noexcept : policy_(std::move(policy)) {}

  MpscRing(MpscRing&& other) noexcept { moveFrom(std::move(other)); }
  MpscRing& operator=(MpscRing&& other) noexcept {
    if (this != &other) {
      if (reader_attached_) detail::misuseTrap("moving over an attached MpscRing reader");
      moveFrom(std::move(other));
    }
    return *this;
  }
  MpscRing(MpscRing const&) = delete;
  MpscRing& operator=(MpscRing const&) = delete;

  [[nodiscard]] static bool create(Config const& cfg, MpscRing& out) noexcept {
    Config c = cfg;
    c.shards = 1;
    c.plane_claim_stride = kClaimStride;
    c.plane_result_stride = kResultStride;
    c.plane_grain = kCellGrain;
    if (!Region::create(c, out.owned_)) return false;
    out.bind(out.owned_, 0);
    return true;
  }

  // Attaches to a region created by another process. This is the whole point of
  // the planes living in the region: the descriptor carries the metadata as
  // well as the payload, so a peer needs nothing but the fd.
  //
  // The stride check is not defensive padding -- a compact binary attaching to
  // a padded region (or the reverse) would index the Claim plane with the wrong
  // stride and silently read a neighbouring record's ownership word.
  [[nodiscard]] static bool attach(int fd, MpscRing& out) noexcept {
    // Attach to a DUPLICATE, never to the caller's descriptor.
    //
    // Region::attach takes ownership on success and closes on destruction, so
    // rejecting the geometry below would otherwise close a descriptor the
    // caller still owns -- and the caller cannot know that a *failed* attach
    // consumed its fd. That is not hypothetical: it made a subsequent valid
    // attach fail in the layout-mismatch test.
    //
    // Duplicating also makes attach() safe to call speculatively, which is what
    // a caller probing for the right cell layout actually wants to do.
    int const dup_fd = ::dup(fd);
    if (dup_fd < 0) return false;
    Region r;
    if (!Region::attach(dup_fd, r)) {
      int const saved = errno;
      ::close(dup_fd);
      errno = saved;
      return false;
    }
    if (r.claimStride() != kClaimStride || r.resultStride() != kResultStride ||
        r.planeGrain() != kCellGrain) {
      errno = EPROTO;
      return false;  // r's destructor closes dup_fd; the caller's fd is intact
    }
    out.owned_ = std::move(r);
    out.bind(out.owned_, 0);
    return true;
  }

  // A second VIEW over an existing ring's region and planes. Owns neither
  // mapping, so `owner` must outlive it.
  //
  // This is what a replacement reader needs: the reader cursor is not in the
  // shared planes, it is view-private (`rd_`), and attachReader() reseeds it
  // from the shared read_pos. Without a second view there is no way to express
  // "the reader died and a new one took over" in-process, and therefore no way
  // to test at-least-once redelivery of a peeked-but-unpopped record.
  //
  // The view takes a FRESH epoch, so a thread holding a reservation on the
  // owner is correctly seen as holding it on a different binding.
  [[nodiscard]] static bool createView(MpscRing& owner, MpscRing& out) noexcept {
    if (owner.arena_ == nullptr || owner.claim_plane_ == nullptr) return false;
    out.bind(owner.owned_, 0);
    return true;
  }

  // ---- writer --------------------------------------------------------------

  PGT_MPSC_HOT inline WriteSpan reserve(sz_t n) noexcept {
    WriterTls& t = tls_;
    assert(t.p == detail::kInvalidPos && "reserve while a reservation is held");
    if (t.owner_epoch != epoch_) [[unlikely]] {
      if (t.p != detail::kInvalidPos) [[unlikely]]
        detail::misuseTrap("reserve while holding a reservation on another MpscRing");
      t.owner_epoch = epoch_;
      t.read_cache = 0;  // conservative in the SAFE direction; see in-band queue
    }
    if (t.tid == 0) [[unlikely]] {
      registerAtfork();
      t.tid = currentTid();
    }
    // Order matters: the representability check comes FIRST, because rounding an
    // unrepresentable length destroys the evidence that it was too large.
    u64_t need = 0;
    if (!tpExtentForChecked<CellGrain>(n, need) || need + kCellGrain > cap_) [[unlikely]] {
      t.status = Status::kTooLarge;
      return {};
    }
    // Same fast path as in-band queue, and sound for the same reason: the CAS's expected
    // value is freeWord(p), so success proves p was the frontier by content
    // alone. No preliminary load, and the failure value is never reused.
    u64_t const p = hintRef().load(std::memory_order_acquire);
    if (p >= t.read_cache && p + need + kCellGrain - t.read_cache <= cap_) [[likely]] {
      u64_t expected = freeWord(p);
      u64_t const mine = packRecord(need, 0, State::kClaimed, t.tid);
      if (claimRef(p).compare_exchange_strong(expected, mine, std::memory_order_acquire,
                                              std::memory_order_relaxed)) [[likely]] {
        return finishClaim(p, need, n, mine, /*hops=*/0);
      }
      noteClaimFailure(policy_, /*fast_path=*/true, /*prior_failures=*/0);
      casFailureBackoff(/*prior_failures=*/0);
      return reserveSlow(n, need, /*initial_prior_failures=*/1);
    }
    return reserveSlow(n, need);
  }

  void commit(WriteSpan reservation, sz_t actual_n) noexcept {
    WriterTls& t = tls_;
    if (t.p == detail::kInvalidPos) [[unlikely]]
      detail::misuseTrap("commit without a live reservation (double commit?)");
    if (t.owner_epoch != epoch_) [[unlikely]]
      detail::misuseTrap("commit on a queue this thread holds no reservation on");
    if (reservation.data() != arena_ + (t.p & mask_) || reservation.size() != t.n)
      detail::misuseTrap("commit with a span other than the live reservation");
    if (actual_n > t.n) [[unlikely]]
      detail::misuseTrap("committed payload exceeds the reservation");
    assert(currentTid() == tidOf(t.word) && "commit across fork or from wrong thread");
    // The Claim word is NOT touched: it keeps the full reserved extent, which
    // is the boundary every walker and the reader advance by. Only Result
    // moves, and the length is published by the tag's release.
    ResultCell& cell = resultCell(t.p);
    std::atomic_ref<u64_t>(cell.len).store(actual_n, std::memory_order_relaxed);
    std::atomic_ref<u64_t>(cell.tag).store(commitTag(t.p),
                                           std::memory_order_release);  // publishes the payload
    policy_.onCommit(t.p, t.need);
    t.p = detail::kInvalidPos;  // poison
  }

  void abort() noexcept {
    WriterTls& t = tls_;
    if (t.p == detail::kInvalidPos) [[unlikely]]
      detail::misuseTrap("abort without a live reservation (double abort?)");
    if (t.owner_epoch != epoch_) [[unlikely]]
      detail::misuseTrap("abort on a queue this thread holds no reservation on");
    assert(currentTid() == tidOf(t.word) && "abort across fork or from wrong thread");
    claimRef(t.p).store(withState(t.word, State::kAborted), std::memory_order_release);
    policy_.onAbort(t.p, t.need);
    t.p = detail::kInvalidPos;  // poison
  }

  bool write(void const* data, sz_t n) noexcept {
    WriteSpan const s = reserve(n);
    if (s.data() == nullptr) return false;
    if (n != 0) std::memcpy(s.data(), data, n);
    commit(s, n);
    return true;
  }

  [[nodiscard]] Status status() const noexcept { return tls_.status; }
  [[nodiscard]] u32_t maxWriters() const noexcept { return kUnboundedWriters; }

  // ---- reader --------------------------------------------------------------

  [[nodiscard]] ReadSpan peek() noexcept {
    for (;;) {
      u64_t const w = claimRef(rd_).load(std::memory_order_acquire);
      u64_t const s = w & kStateMask;
      if (s == kTpFree) return {};  // frontier (only freeWord(rd_) can be here)
      if (s == kTpAborted) {
        peek_extent_ = extentOf(w);
        pop();
        continue;
      }
      // kClaimed or kCleared: in flight unless Result says otherwise.
      ResultCell& cell = resultCell(rd_);
      if (std::atomic_ref<u64_t>(cell.tag).load(std::memory_order_acquire) == commitTag(rd_)) {
        peek_extent_ = extentOf(w);
        u64_t const n = std::atomic_ref<u64_t>(cell.len).load(std::memory_order_relaxed);
        return {arena_ + (rd_ & mask_), n};
      }
      // Liveness stays OFF the busy path, and is SPACED rather than timed --
      // identical doctrine and identical constants to in-band queue.
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
      if (threadAlive(tidOf(w))) {
        if (busy_threshold_ < kBusyPollsPerLivenessMax) busy_threshold_ *= 2;
        policy_.onBusy(busy_threshold_);
        return {};
      }
      // MUST re-read after the liveness verdict. Between the loads above and
      // the /proc check the owner can have committed AND died, so both the
      // Claim word and the Result tag are stale snapshots. Re-read Result
      // first: a committed record must be delivered, never recovered.
      if (std::atomic_ref<u64_t>(cell.tag).load(std::memory_order_acquire) == commitTag(rd_)) {
        continue;  // re-dispatch; the committed branch above will deliver it
      }
      u64_t const fresh = claimRef(rd_).load(std::memory_order_acquire);
      u64_t const fs = fresh & kStateMask;
      if (fs == kTpClaimed || fs == kTpCleared) recover(rd_, fresh);
      continue;
    }
  }

  [[nodiscard]] bool maybeReadable() const noexcept {
    return (claimRef(rd_).load(std::memory_order_relaxed) & kStateMask) != kTpFree;
  }

  void pop() noexcept {
    u64_t const prev = rd_;
    u64_t const extent =
      peek_extent_ != 0 ? peek_extent_ : extentOf(claimRef(rd_).load(std::memory_order_relaxed));
    peek_extent_ = 0;
    rd_ += extent;                                       // reader-private
    readPosRef().store(rd_, std::memory_order_release);  // license to overwrite
    if ((prev ^ rd_) >> shift_) policy_.onWrap(rd_ >> shift_);
  }

  // ---- attachment (cold) ---------------------------------------------------

  [[nodiscard]] bool attachWriter() noexcept {
    if (arena_ == nullptr) return false;
    registerAtfork();
    return true;
  }

  [[nodiscard]] bool attachReader() noexcept {
    if (arena_ == nullptr) return false;
    auto rt = readerTidRef();
    u32_t const self = currentTid();
    u32_t cur = rt.load(std::memory_order_acquire);
    // Idempotent only for this view. The shared ABI stores a TID, not a view
    // token, so a second object in the same thread must not be allowed to seed a
    // separate rd_ from the same shared read_pos and split the stream. A future
    // ABI could add a start-time/token field; this one cannot distinguish a dead
    // reader from a later kernel TID reuse.
    if (cur == self && !reader_attached_) return false;
    if (cur != self) {
      if (cur != 0 && threadAlive(cur)) return false;
      if (!rt.compare_exchange_strong(cur, self, std::memory_order_acq_rel,
                                      std::memory_order_relaxed)) {
        return false;
      }
    }
    reader_attached_ = true;
    rd_ = readPosRef().load(std::memory_order_acquire);
    peek_extent_ = 0;
    return true;
  }

  // Reader ownership is explicit. Destruction does not silently detach: that
  // preserves crash/restart semantics, leaving successors to prove TID death.
  // Voluntary same-thread handoff calls detachReader(); a peeked but unpopped
  // record remains licensed for at-least-once redelivery because read_pos did
  // not advance.
  [[gnu::noinline, gnu::cold]] void detachReader() noexcept {
    if (!reader_attached_) [[unlikely]]
      detail::misuseTrap("MpscRing::detachReader by a view that is not attached");
    u32_t expected = currentTid();
    if (!readerTidRef().compare_exchange_strong(expected, 0, std::memory_order_acq_rel,
                                                std::memory_order_relaxed)) {
      detail::misuseTrap("MpscRing::detachReader by a thread that does not own the reader slot");
    }
    reader_attached_ = false;
    peek_extent_ = 0;
  }

  void detachWriter() noexcept {
    if (tls_.p != detail::kInvalidPos && tls_.owner_epoch == epoch_) abort();
  }

  [[nodiscard]] Region const& region() const noexcept { return owned_; }
  [[nodiscard]] Policy& policy() noexcept { return policy_; }

  // Metadata bytes per byte of payload ring, for the overhead table.
  [[nodiscard]] static constexpr double metadataRatio() noexcept {
    return static_cast<double>(kClaimStride + kResultStride) / kCellGrain;
  }

 private:
  struct WriterTls {
    u64_t p = detail::kInvalidPos;
    u64_t need = 0;
    u64_t n = 0;
    u64_t word = 0;
    u64_t read_cache = 0;
    u64_t owner_epoch = 0;
    u32_t tid = 0;
    Status status = Status::kOk;
  };
  inline static thread_local WriterTls tls_{};

  static void atforkChild() noexcept { tls_ = WriterTls{}; }
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
    cell_mask_ = (cap_ / kCellGrain) - 1;
    shift_ = static_cast<u32_t>(std::countr_zero(cap_));
    claim_plane_ = region.claimPlane(shard_index);
    result_plane_ = region.resultPlane(shard_index);
    static std::atomic<u64_t> ctr{0};
    epoch_ = ctr.fetch_add(1, std::memory_order_relaxed) + 1;
  }

  void moveFrom(MpscRing&& other) noexcept {
    owned_ = std::move(other.owned_);
    ctl_ = other.ctl_;
    sc_ = other.sc_;
    arena_ = other.arena_;
    claim_plane_ = other.claim_plane_;
    result_plane_ = other.result_plane_;
    cap_ = other.cap_;
    mask_ = other.mask_;
    cell_mask_ = other.cell_mask_;
    shift_ = other.shift_;
    epoch_ = other.epoch_;
    rd_ = other.rd_;
    peek_extent_ = other.peek_extent_;
    reader_attached_ = other.reader_attached_;
    busy_pos_ = other.busy_pos_;
    busy_streak_ = other.busy_streak_;
    busy_threshold_ = other.busy_threshold_;
    policy_ = std::move(other.policy_);

    other.ctl_ = nullptr;
    other.sc_ = nullptr;
    other.arena_ = nullptr;
    other.claim_plane_ = nullptr;
    other.result_plane_ = nullptr;
    other.epoch_ = 0;
    other.reader_attached_ = false;
  }

  [[nodiscard]] u64_t cellOf(u64_t pos) const noexcept { return (pos / kCellGrain) & cell_mask_; }

  [[nodiscard]] std::atomic_ref<u64_t> claimRef(u64_t pos) const noexcept {
    return std::atomic_ref<u64_t>(
      *reinterpret_cast<u64_t*>(claim_plane_ + cellOf(pos) * kClaimStride));
  }
  [[nodiscard]] ResultCell& resultCell(u64_t pos) const noexcept {
    return *reinterpret_cast<ResultCell*>(result_plane_ + cellOf(pos) * kResultStride);
  }
  [[nodiscard]] std::atomic_ref<u64_t> hintRef() const noexcept {
    return std::atomic_ref<u64_t>(sc_->writeHint());
  }
  [[nodiscard]] std::atomic_ref<u64_t> readPosRef() const noexcept {
    return std::atomic_ref<u64_t>(sc_->read_pos);
  }
  [[nodiscard]] std::atomic_ref<u32_t> readerTidRef() const noexcept {
    return std::atomic_ref<u32_t>(ctl_->reader_tid);
  }

  static void casFailureBackoff(u32_t prior_failures) noexcept {
    if constexpr (kUsesCasFailureBackoff<Policy>) {
      u32_t const pauses = casFailureBackoffPauses(prior_failures);
      for (u32_t i = 0; i < pauses; ++i) cpuRelax();
    } else {
      static_cast<void>(prior_failures);
    }
  }

  // Promote the successor cell RECYCLED-or-stale -> FREE(q). Exclusive by the
  // same argument as in-band queue's stampSuccessorFree: q cannot be claimed until this
  // store lands (a claim needs the exact freeWord(q)), q cannot be recycled
  // while this record is uncommitted (I3), and the admission slack guarantees
  // cell(q) is not still owned by a live record of the previous lap.
  //
  // Relaxed: it publishes no payload. The vouch's release makes it visible to
  // the reader, which is its only consumer.
  PGT_MPSC_HOT inline void promoteSuccessor(u64_t q) const noexcept {
    claimRef(q).store(freeWord(q), std::memory_order_relaxed);
  }

  PGT_MPSC_HOT inline WriteSpan finishClaim(u64_t p, u64_t need, sz_t n, u64_t mine,
                                            u32_t hops) noexcept {
    u64_t const q = p + need;
    promoteSuccessor(q);
    // The vouch. One release store, three consumers -- all of the same fact,
    // "the successor has been promoted and is now public":
    //
    //   1. It is the PUBLICATION EDGE for the relaxed promotion above. A
    //      walker's acquire load of Claim[p] synchronizes-with this store, which
    //      is what makes freeWord(q) visible to it.
    //   2. It is the CLAIM LICENCE. A walker may advance past a kClaimed record
    //      but may not claim its successor; leaving kClaimed is what lifts that.
    //   3. It tells RECOVERY not to re-promote: kClaimed means the owner never
    //      got here, anything else means it did and someone may now own q.
    //
    // This transition is also what makes the successor guard nearly free. A
    // record reads kClaimed only between the promotion and this store -- two
    // stores, closed before reserve() returns -- rather than for the whole
    // caller-controlled reservation. Without it the guard would block a
    // successor for the reservation's lifetime, which is in-band queue's behaviour and
    // in-band queue's collapse.
    //
    // Note this store lands on the line the CAS above already holds Modified,
    // unlike in-band queue's cold successor line.
    claimRef(p).store(withState(mine, State::kCleared), std::memory_order_release);  // vouch (I2)
    hintRef().store(q, std::memory_order_release);
    tls_.p = p;
    tls_.need = need;
    tls_.n = n;
    tls_.word = mine;
    tls_.status = Status::kOk;
    policy_.onClaim(p, need, hops);
    return {arena_ + (p & mask_), n};
  }

  [[gnu::noinline, gnu::cold]] WriteSpan reserveSlow(sz_t n, u64_t need,
                                                     u32_t initial_prior_failures = 0) noexcept {
    WriterTls& t = tls_;
    u32_t const max_hops = static_cast<u32_t>(cap_ / kCellGrain);
    u32_t contended = initial_prior_failures;
    for (;;) {
      u64_t p = hintRef().load(std::memory_order_acquire);
      // Whether the record immediately preceding the current position was
      // observed kClaimed. Seeded false: the hint is published AFTER the vouch,
      // so a position reached from the hint always has a vouched predecessor.
      bool prev_claimed = false;
      u32_t hops = 0;
      for (;;) {
        if (++hops > max_hops) goto restart;  // bounded: stale walk
        u64_t const w = claimRef(p).load(std::memory_order_acquire);
        u64_t const s = w & kStateMask;
        if (s == kTpFree) {
          if (!isFreeFor(w, p)) goto restart;  // wrong lap: stale walk
          // THE SUCCESSOR OF A kClaimed RECORD IS NOT CLAIMABLE.
          //
          // This is what lets recovery promote unconditionally. Recovery fires
          // on a record still kClaimed with a dead owner; that record has been
          // kClaimed since its CAS, so it was never observed otherwise, so no
          // walker can have claimed its successor -- there is nothing at q to
          // destroy and nothing to classify. It restores exactly the invariant
          // the in-band ring got from blocking, without blocking the walk.
          //
          // Cheap because kClaimed is only the promote-then-vouch window inside
          // finishClaim, two stores, closed before reserve() returns. The long
          // caller-controlled window is kCleared. The one case that blocks for
          // real is a DEAD owner, which is precisely when blocking is correct.
          //
          // Monotonic states make the observation safe to act on: once seen
          // kCleared a record never returns to kClaimed, so a claim licensed
          // here stays licensed.
          if (prev_claimed) goto restart;
          break;  // frontier
        }
        prev_claimed = (s == kTpClaimed);
        // kClaimed / kCleared / kAborted all carry a full extent, and unlike
        // in-band queue a kClaimed record does NOT block the walk: the successor cell
        // holds a well-formed claim word (this lap's or an older one), never
        // raw payload, and Lemma 1 still makes a bad claim impossible.
        {
          u64_t const e = extentOf(w);
          if (e == 0) goto restart;  // stale/garbage word; never advance by zero
          p += e;
        }
      }

      if (p < t.read_cache) goto restart;  // stale walk, NOT full
      if (p + need + kCellGrain - t.read_cache > cap_) {
        t.read_cache = readPosRef().load(std::memory_order_acquire);
        if (p < t.read_cache) goto restart;
        if (p + need + kCellGrain - t.read_cache > cap_) {
          policy_.onFull(0);
          u32_t const reader = readerTidRef().load(std::memory_order_acquire);
          t.status = (reader != 0 && !threadAlive(reader)) ? Status::kReaderDead : Status::kFull;
          return {};  // fail fast
        }
      }

      {
        u64_t expected = freeWord(p);
        u64_t const mine = packRecord(need, 0, State::kClaimed, t.tid);
        if (claimRef(p).compare_exchange_strong(expected, mine, std::memory_order_acquire,
                                                std::memory_order_relaxed)) {
          return finishClaim(p, need, n, mine, hops);
        }
        noteClaimFailure(policy_, /*fast_path=*/false, contended);
        casFailureBackoff(contended);
        policy_.onContended(contended++);
      }
    restart:;
    }
  }

  // Reader-only recovery of a proven-dead writer's record. `w` MUST have been
  // read AFTER the liveness verdict, and the Result tag MUST have been
  // re-checked first -- an owner that committed and then died owns a delivered
  // record, not a recoverable one.
  [[gnu::noinline, gnu::cold]] void recover(u64_t p, u64_t w) noexcept {
    if ((w & kStateMask) == kTpClaimed) {
      // Unconditional and sound: no walker may claim the successor of a record
      // it observed kClaimed (see reserveSlow), and this record has held that
      // state since its claim CAS. Cell(q) is therefore either freeWord(q)
      // already, if the owner promoted before dying, or a stale word nobody
      // owns. Classification schemes based on lap parity, retirement stamps,
      // and chain following are rejected in docs/mpsc-queue.md.
      promoteSuccessor(p + extentOf(w));
    }
    claimRef(p).store(withState(w, State::kAborted), std::memory_order_release);
    policy_.onReclaim(p, tidOf(w), extentOf(w));
  }

  Region owned_;
  Control* ctl_ = nullptr;
  ShardControl* sc_ = nullptr;
  std::byte* arena_ = nullptr;
  std::byte* claim_plane_ = nullptr;
  std::byte* result_plane_ = nullptr;
  u64_t cap_ = 0;
  u64_t mask_ = 0;
  u64_t cell_mask_ = 0;
  u32_t shift_ = 0;
  u64_t epoch_ = 0;
  // Reader-written state on its own line. cap_/mask_/cell_mask_ are read by
  // EVERY writer on every claim; rd_/peek_extent_ are stored by the reader on
  // every pop. Sharing one line makes each reader store invalidate that line in
  // all N writers, so the cost grows with writer count.
  alignas(64) u64_t rd_ = 0;
  u64_t peek_extent_ = 0;
  bool reader_attached_ = false;

  static constexpr u32_t kBusyPollsPerLiveness = 128;
  static constexpr u32_t kBusyPollsPerLivenessMax = 1u << 16;
  u64_t busy_pos_ = detail::kInvalidPos;
  u32_t busy_streak_ = 0;
  u32_t busy_threshold_ = kBusyPollsPerLiveness;

  [[no_unique_address]] Policy policy_{};
};

static_assert(QueueLike<MpscRing<DefaultPolicy, false>>);
static_assert(QueueLike<MpscRing<DefaultPolicy, true>>);
static_assert(QueueLike<MpscRing<DefaultPolicy, true, 256>>);
static_assert(std::atomic_ref<u32_t>::is_always_lock_free);
static_assert(std::atomic_ref<u64_t>::is_always_lock_free);

}  // namespace pgt::mpsc
