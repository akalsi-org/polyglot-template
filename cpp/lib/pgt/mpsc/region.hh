#pragma once

// The mirrored ring mapping, shared by every queue variant.
//
// ONE file and ONE reservation regardless of shard count. The layout is:
//
//   file offsets                    virtual address space
//   ------------                    ---------------------
//   control page   -- mmap x1 -->   control            (canonical alias ONLY)
//   arena 0 (N)  --+- mmap ---->    arena 0     base + 0
//                  +- mmap ---->    arena 0'    base + N      <- same pages
//   arena 1 (N)  --+- mmap ---->    arena 1     base + 2N
//                  +- mmap ---->    arena 1'    base + 3N
//   ...
//
// Each arena is mapped twice, back to back, so a record wrapping the end of the
// ring is still one contiguous span. That single property is the entire
// justification for the mapping complexity: reserve() always returns one span
// and peek() always returns one span, with no padding records, no bipartite
// split, and no "does not fit before the end, skip to zero" logic.
//
// Both mappings of an arena are MAP_FIXED into one pre-reserved PROT_NONE hole
// covering the whole layout. The reservation is what makes the pair race-free:
// without it another thread's unrelated mmap can land between the two MAP_FIXED
// calls.
//
// CONTROL WORDS LIVE IN THE CONTROL PAGE ONLY, which is mapped once. Never place
// an atomic in a mirrored arena: the two aliases are DIFFERENT LOCATIONS to the
// compiler, which may reorder or combine relaxed accesses that in fact touch the
// same physical word. That is a miscompilation hazard, not a coherence one --
// the hardware handles the aliasing fine (x86 L1D is physically tagged, ARMv8
// requires PIPT behaviour).
//
// Payload access should also stay on ONE alias per record: the two aliases differ
// by a page multiple, so their low 12 address bits are identical, making a
// cross-alias store/load pair a guaranteed 4K-aliasing false dependency on x86.
//
// Positions are unwrapped monotonic u64 everywhere. Masking happens only at
// address computation, which removes all wrap ambiguity from comparisons and is
// what lets the descriptor encoding distinguish laps.

#include "pgt/core/types.hh"
#include "pgt/mpsc/api.hh"

#include <cstddef>

namespace pgt::mpsc {

// Per-shard control block. Each cursor gets its own 128-byte line: the coherence
// granule on x86-64 is 64, but Intel's L2 adjacent-line prefetcher pairs lines,
// and there are only a couple of hot cursors per shard so the padding is free.
// Per-record padding is not free, which is why the record grain stays at 64.
struct alignas(128) ShardControl {
  // One word, two contracts, aliased DELIBERATELY -- use the accessor that names
  // the contract in force:
  //
  //   writeHint()    shared-ring variants. A hint only, never authoritative: it
  //                  may lag (costing walk hops) but must never lead (which
  //                  would skip records).
  //   publishPos()   SPSC rings. The authoritative publication tail; never lags.
  //
  // Same storage because each variant uses exactly one of the two, and giving
  // the word a single name for both contracts proved to be a trap.
  u64_t write_hint;
  std::byte pad0[128 - sizeof(u64_t)];

  // Reader-written only.
  u64_t read_pos;
  std::byte pad1[128 - sizeof(u64_t)];

  [[nodiscard]] u64_t& writeHint() noexcept { return write_hint; }
  [[nodiscard]] u64_t& publishPos() noexcept { return write_hint; }
};

// Queue-wide control page. Read-only after init except for the reader slot.
struct alignas(128) Control {
  u64_t magic;
  u32_t version;
  u32_t shards;
  u64_t capacity;  // bytes per shard arena
  u32_t tgid;      // creator's thread-group id; a tgkill fast path for local use

  // Written once when a reader attaches, and CAS'd on takeover. Serves two jobs,
  // both using exact liveness rather than a timeout: a writer facing persistent
  // backpressure can tell a dead reader from a slow one, and a successor reader
  // uses it to claim the role. Checked only on cold paths.
  u32_t reader_tid;

  std::byte pad[128 - 32];
};

// One entry per writer slot in the shared registry. The registry MUST live in
// the shared control area, not in any queue object: slot arbitration is
// cross-process, and an instance-local table would let two processes claim the
// same slot -- for an SPSC ring, whose correctness argument is "sole ring
// owner", that is silent corruption.
struct WriterSlot {
  u32_t owner_tid;
  // Bumped when the slot is recycled after its owner is PROVEN dead, so a
  // stale actor holding the old {slot, generation} cannot resurrect into a
  // reused slot.
  u32_t generation;
};

// Owns the fd and the mapping. Move-only; unmaps and closes on destruction.
class Region {
 public:
  Region() = default;
  ~Region();
  Region(Region&&) noexcept;
  Region& operator=(Region&&) noexcept;
  Region(Region const&) = delete;
  Region& operator=(Region const&) = delete;

  // Creates the backing object, seals it where the backend allows, optionally
  // preallocates, and installs the mirrored mapping.
  //
  // Seals are applied before the descriptor is ever shared. F_SEAL_SHRINK is the
  // one that matters even inside a cooperative trust boundary: the failure mode
  // is a *crashing* peer, not a hostile one, and a peer's ftruncate is the most
  // common way a shared-memory queue dies. Only memfd supports it.
  [[nodiscard]] static bool create(Config const& cfg, Region& out) noexcept;

  // Attaches to an existing region by descriptor, verifying magic and version.
  [[nodiscard]] static bool attach(int fd, Region& out) noexcept;

  [[nodiscard]] Control* control() const noexcept { return control_; }
  [[nodiscard]] ShardControl* shard(u32_t i) const noexcept { return shards_ + i; }

  // Writer-slot registry, in the control area right after the ShardControls.
  // Occupancy is one bit per slot; claim a slot with a wait-free fetch_or over
  // the words (first free bit won wins, bounded by the table size). Do NOT
  // "simplify" that into a CAS loop -- the fetch_or is what makes registration
  // wait-free rather than lock-free.
  [[nodiscard]] u64_t* writerBitmap() const noexcept {
    return reinterpret_cast<u64_t*>(shards_ + shard_count_);
  }
  [[nodiscard]] u32_t bitmapWords() const noexcept { return (shard_count_ + 63) / 64; }
  [[nodiscard]] WriterSlot* writerSlots() const noexcept {
    return reinterpret_cast<WriterSlot*>(writerBitmap() + bitmapWords());
  }

  // Base of shard i's primary mapping. Address a position with
  // `arena(i) + (pos & mask())`; the mirror makes any span of at most
  // capacity bytes contiguous from there.
  [[nodiscard]] std::byte* arena(u32_t i) const noexcept {
    return arena_ + static_cast<sz_t>(i) * 2 * capacity_;
  }

  [[nodiscard]] u64_t capacity() const noexcept { return capacity_; }
  [[nodiscard]] u64_t mask() const noexcept { return capacity_ - 1; }
  [[nodiscard]] u32_t shardCount() const noexcept { return shard_count_; }
  [[nodiscard]] int fd() const noexcept { return fd_; }
  [[nodiscard]] bool valid() const noexcept { return arena_ != nullptr; }

 private:
  void reset() noexcept;

  int fd_ = -1;
  std::byte* base_ = nullptr;  // whole reservation, for munmap
  sz_t reservation_ = 0;
  Control* control_ = nullptr;
  ShardControl* shards_ = nullptr;
  std::byte* arena_ = nullptr;
  u64_t capacity_ = 0;
  u32_t shard_count_ = 0;
};

}  // namespace pgt::mpsc
