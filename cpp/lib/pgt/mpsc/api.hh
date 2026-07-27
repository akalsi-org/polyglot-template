#pragma once

// The contract every queue variant implements.
//
// Two variants survive:
//
//   TwoPlaneRing  many writers on one arena, one reader, total order across all
//                 writers (two_plane.hh). Arbitration, publication, and payload
//                 live on SEPARATE cache lines: a Claim cell carries ownership,
//                 a Result cell carries completion, and the payload is touched
//                 only by its owner and then the reader. Claim-by-CAS on the
//                 self-certifying FREE(p) word; reader-only recovery gated on
//                 proven death.
//
//   SpscRing      ONE writer on one arena. No claim arbitration: reserve is a
//                 plain store, so it is WAIT-FREE rather than lock-free. The
//                 tail is a publication cursor, so the reader never observes an
//                 incomplete record and there is NO recovery protocol at all --
//                 a dead writer's ring simply idles. Fastest, weakest ordering.
//
// The in-band shared ring (`Ring`/`Mpsc`) and the `Sharded` composition over it
// (`ShardedMpsc`, `MultiSpsc`) were REMOVED. `Ring` put the descriptor in the
// same cache line as the first 56 payload bytes, so the claim CAS, the owner's
// payload writes, and the reader's poll all contended for one line; measured on
// Zen 5 it lost roughly 80% of throughput across the 2->4 writer step and fell
// to 0.04 IPC at eight writers. TwoPlaneRing replaces it rather than tuning it:
// the residual cost was the line sharing, not the claim instruction.
//
// Ordering is the axis they differ on, and it is a property of the variant
// rather than of the API. Nothing here promises a total order; consult the
// variant.
//
// SELECTION
//
//   Single producer                  -> Spsc. Nothing else is close; it does no
//                                       arbitration at all.
//   Two or more producers            -> TwoPlaneMpsc.
//   Metadata footprint is critical   -> Spsc if the topology allows it;
//                                       TwoPlaneRing costs 24 bytes of Claim +
//                                       Result per 64 bytes of ring (37.5%).
//
// MEASURED, Zen 5, 56-byte payloads, 25 interleaved processes per point, medians
// (see docs/mpsc-queue.md for intervals and method):
//
//   writers               1       2       4       8      16
//   TwoPlaneMpsc       41.9    39.9    36.8    15.5     9.7   Mrec/s
//   misses/record       1.3     1.4     1.7     3.2     5.3
//   IPC                1.30    1.35    1.35    1.28    1.33
//
// The design does not scale UP -- one total order still means one successful
// claim per record -- but it degrades gracefully instead of collapsing. The
// padded cell layout (TwoPlaneMpscPadded) is SLOWER above two writers despite
// its 200% metadata cost: giving every cell its own line spreads the working set
// further than it saves in false sharing. Compact is the default for that
// reason, and the padded layout is retained only as a measurement control.
//
// Machine load is the dominant error term in any of these numbers: an earlier
// campaign run while other processes compiled was wrong by 3.5x and inverted a
// headline result. Re-measure on a quiet box before trusting any change.
//
// REGISTRATION ASYMMETRY, which is a real difference callers must handle:
//
//   TwoPlaneMpsc  no registration. Any thread may write; attachWriter() cannot
//                 fail. Writers are unbounded.
//   Spsc          registration is EXCLUSIVE and ENFORCES the single-writer
//                 contract: a second thread's attachWriter() FAILS rather than
//                 silently creating a second writer. maxWriters() == 1.
//
// Spsc enforcing rather than merely documenting its contract is deliberate. Two
// writers on an SPSC ring is the one misuse that silently corrupts -- the whole
// algorithm (no claim CAS, publication-cursor semantics, no recovery path) rests
// on sole ownership. A cold-path slot claim, arbitrated through the Region's
// SHARED writer registry so it holds cross-process, turns that into a detected
// failure for free.
//
// maxWriters() bounds LIVE writers, not attach() successes over a lifetime. A
// writer that exits releases its ring: once its owner is provably dead AND the
// ring has drained, the slot is recycled and a new writer may claim it. So a
// maxWriters() bounds LIVE writers, not attach() successes over a lifetime: a
// slot freed by a departed writer is reusable, so a bounded queue admits new
// writers indefinitely as old ones leave. Only Spsc has a writer capacity, and
// it is 1. TwoPlaneRing reports kUnboundedWriters.

#include "../core/types.hh"

#include <cstddef>
#include <span>

namespace pgt::mpsc {

enum class Backend {
  // Anonymous, sealable, passed by fd. The default. Only backend that supports
  // F_SEAL_SHRINK, which is what stops a peer's ftruncate from SIGBUS-ing the
  // reader -- the most common way a shared-memory queue dies in production.
  kMemfd,
  // Named under /dev/shm. Loses seals, gains a name a successor can open
  // unaided: the one thing that makes reader restart work without depending on
  // a surviving writer or a supervisor holding the fd.
  kShm,
  // Explicit path. Almost always wrong for a queue on a real filesystem --
  // MAP_SHARED means continuous writeback for data nobody intends to persist.
  // Supported so a crashed ring can be inspected post-mortem.
  kFile,
};

struct Config {
  // Bytes of ring per shard. Rounded up to a power of two and a multiple of the
  // page size.
  //
  // MEASURED: capacity does NOT affect push latency. Push cost is flat (~70ns on
  // Zen 5) from 256 KiB to 256 MiB, because while the reader keeps pace the hot
  // working set is the moving frontier window -- a few KiB -- which is cache
  // resident at any ring size. The ring as a whole is never the working set.
  //
  // What capacity buys is BACKLOG TOLERANCE: how far the reader may fall behind
  // before writers see backpressure. A cold backlog then streams out at DRAM
  // bandwidth (~28 GB/s measured), so a large ring costs nothing until it is
  // actually used as one.
  //
  // Padding interacts here rather than with throughput: at 64-byte grain a 64 B
  // payload occupies a 128 B extent, so padding waste is a tax on effective
  // capacity -- and therefore on the full-rate under backlog -- not a
  // throughput sawtooth. Records sized 64k-8 (56, 120, 184, ...) pack exactly.
  sz_t capacity = 1u << 20;

  // Shard count. Both surviving variants are standalone single-shard rings, so
  // this stays 1; it remains in the layout because the Region, its mirrored
  // mapping, and the shared writer registry are all written to be K-shard, and
  // narrowing them to K=1 would be a larger change than the variants that used
  // multiple shards were worth.
  u32_t shards = 1;

  Backend backend = Backend::kMemfd;

  // For kShm (a name) or kFile (a path).
  char const* name = nullptr;

  // Preallocate at construction so no first-touch fault can SIGBUS under memory
  // pressure or allocate on the hot path. Turning this off makes the queue no
  // longer allocation-free.
  bool preallocate = true;

  // Metadata-plane geometry for the split control/data-plane variant
  // (TwoPlaneRing). Zero -- the default -- means no planes, and the region
  // layout is then byte-identical to what the in-band variants have always
  // used, so `Ring` and the SPSC rings are unaffected.
  //
  // Set by TwoPlaneRing::create() from its own compile-time strides. It lives
  // in Config rather than being inferred because the compact and padded layouts
  // differ only in stride, and the region has to size the file before any queue
  // object exists.
  //
  // The planes are placed in the SINGLY-MAPPED control area, never in a
  // mirrored arena: Claim and Result cells are atomics, and the two aliases of a
  // mirrored arena are different locations to the compiler, which may reorder or
  // combine relaxed accesses that in fact touch the same physical word. That is
  // a miscompilation hazard and the reason this geometry cannot simply be carved
  // out of the ring.
  u64_t plane_claim_stride = 0;
  u64_t plane_result_stride = 0;
};

// Returned by reserve(): empty when the queue cannot accept the record. commit()
// takes this exact pointer-and-length span plus an actual length no greater than
// its size, so same-grain over-commits and accidental subspans are rejected.
using WriteSpan = std::span<std::byte>;
using ReadSpan = std::span<std::byte const>;

enum class Status : u8_t {
  kOk = 0,
  kFull,        // backpressure; retry later
  kReaderDead,  // no reader will ever drain: a definite error, not backpressure
  kTooLarge,    // exceeds what this configuration can ever hold
  kNoSlot,      // writer registration failed (the Spsc slot is taken)
};

// Writer capacity of a variant with no registration limit.
inline constexpr u32_t kUnboundedWriters = 0xffffffffu;

// clang-format off
template <typename Q>
concept QueueLike = requires(Q q, void const* p, sz_t n, u32_t slot) {
  // Writer capacity. kUnboundedWriters unless registration is exclusive.
  { q.maxWriters() } -> std::same_as<u32_t>;

  // Writer. reserve() returns an empty span on failure; status() reports why.
  { q.reserve(n) }        -> std::same_as<WriteSpan>;
  { q.commit(WriteSpan{}, n) } -> std::same_as<void>;
  { q.abort() }           -> std::same_as<void>;
  { q.write(p, n) }       -> std::same_as<bool>;
  { q.status() }          -> std::same_as<Status>;

  // Reader. peek() returns an empty span when nothing is deliverable yet.
  { q.peek() }            -> std::same_as<ReadSpan>;
  { q.pop() }             -> std::same_as<void>;

  // Attachment. Both are cold paths. attachReader() enforces exactly one reader
  // and, where the variant supports restart, takes over from a proven-dead
  // predecessor -- resuming exactly at its published read position, since the
  // reader keeps no state that is not in the control page.
  { q.attachWriter() }    -> std::same_as<bool>;
  { q.attachReader() }    -> std::same_as<bool>;
  { q.detachWriter() }    -> std::same_as<void>;
};
// clang-format on

}  // namespace pgt::mpsc
