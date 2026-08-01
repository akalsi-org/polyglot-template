#pragma once

// The contract every queue variant implements.
//
// Two variants survive:
//
//   MpscRing  many writers on one arena, one reader, total order across all
//             writers (mpsc_ring.hh). Arbitration, publication, and payload
//             live on separate cache lines: a Claim cell carries ownership, a
//             Result cell carries completion, and only the owner then reader
//             touch payload. Claims use self-certifying FREE(p) words; reader-
//             only recovery is gated on proven death.
//
//   SpscRing  one writer on one arena. With no claim arbitration, reserve is a
//             plain store and wait-free. Its tail is a publication cursor, so
//             the reader never observes an incomplete record and a dead writer's
//             ring simply idles.
//
// The removed in-band queue and its sharded compositions placed a descriptor in
// the payload's first cache line. Claim, payload writes, and reader polling then
// contended for that line. MpscRing replaces that layout by separating the three
// jobs, rather than tuning the old claim path.
//
// Ordering is a property of the selected variant; this common API does not
// promise a total order.
//
// SELECTION
//
//   Single producer                  -> SpscRing<>.
//   Two or more producers            -> MpscRing<>.
//   Metadata footprint is critical   -> SpscRing<> if topology allows it;
//                                       MpscRing<> costs 24 metadata bytes per
//                                       64 payload bytes (37.5%).
//
// REGISTRATION ASYMMETRY
//
//   MpscRing<>  no registration. Any thread may write; attachWriter() cannot
//               fail. Writers are unbounded.
//   SpscRing<>  exclusive registration enforces one writer. A second thread's
//               attachWriter() fails rather than silently adding a writer;
//               maxWriters() == 1.
//
// SpscRing<> enforces, rather than merely documents, its single-writer contract:
// two writers could otherwise silently corrupt the queue. Its cold-path slot
// claim lives in Region's shared writer registry, so it also holds cross-process.
//
// maxWriters() bounds live writers, not lifetime attach successes. A slot from a
// proven-dead writer is reusable after its ring drains. SpscRing<> has capacity
// one; MpscRing<> reports kUnboundedWriters.

#include "pgt/core/types.hh"

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

  // Metadata-plane geometry for the split control/data-plane queues. Zero --
  // the default -- means no planes. MpscRing configures Claim + Result planes;
  // SpscRing configures one compact per-position length plane.
  //
  // Set by each queue's create() from its compile-time strides. It lives in
  // Config rather than being inferred because the region has to size the file
  // before any queue object exists.
  //
  // plane_grain is the byte distance represented by one metadata cell.
  // Zero preserves the no-plane geometry; with planes and a zero value,
  // Region::create() records the historical 64-byte grain. A larger grain is
  // only valid for queue variants that enforce at least that much record
  // spacing, and shrinks the plane cell count without changing arena capacity.
  //
  // The planes are placed in the SINGLY-MAPPED control area, never in a
  // mirrored arena: Claim and Result cells are atomics, and the two aliases of a
  // mirrored arena are different locations to the compiler, which may reorder or
  // combine relaxed accesses that in fact touch the same physical word. That is
  // a miscompilation hazard and the reason this geometry cannot simply be carved
  // out of the ring.
  u64_t plane_claim_stride = 0;
  u64_t plane_result_stride = 0;
  u64_t plane_grain = 0;
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
  kNoSlot,      // writer registration failed (the SpscRing slot is taken)
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
